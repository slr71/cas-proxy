package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/pkg/errors"
	"github.com/rs/cors"
	"github.com/yhat/wsutil"
)

var log = logrus.WithFields(logrus.Fields{
	"service": "cas-proxy",
	"art-id":  "cas-proxy",
	"group":   "org.cyverse",
})

const sessionName = "proxy-session"
const sessionKey = "proxy-session-key"
const sessionAccess = "proxy-session-last-access"

// CASProxy contains the application logic that handles authentication, session
// validations, ticket validation, and request proxying.
type CASProxy struct {
	casBase        string // base URL for the CAS server
	casValidate    string // The path to the validation endpoint on the CAS server.
	frontendURL    string // The URL placed into service query param for CAS.
	backendURL     string // The backend URL to forward to.
	wsbackendURL   string // The websocket URL to forward requests to.
	resourceType   string // The resource type for analysis.
	resourceName   string // The UUID of the analysis.
	ingressURL     string // The URL to the cluster ingress.
	accessHeader   string // The Host header for checking resource access perms.
	analysisHeader string // The Host header for getting the analysis ID.
	sessionStore   *sessions.CookieStore
}

// NewCASProxy returns a newly instantiated *CASProxy.
func NewCASProxy(casBase, casValidate, frontendURL, backendURL, wsbackendURL string, cs *sessions.CookieStore) *CASProxy {
	return &CASProxy{
		casBase:      casBase,
		casValidate:  casValidate,
		frontendURL:  frontendURL,
		backendURL:   backendURL,
		wsbackendURL: wsbackendURL,
		sessionStore: cs,
	}
}

// Analysis contains the ID for the Analysis, which gets used as the resource
// name when checking permissions.
type Analysis struct {
	ID string `json:"id"` // Literally all we care about here.
}

// Analyses is a list of analyses returned by the apps service.
type Analyses struct {
	Analyses []Analysis `json:"analyses"`
}

func (c *CASProxy) getResourceName(externalID string) (string, error) {
	bodymap := map[string]string{}
	bodymap["external_id"] = externalID

	body, err := json.Marshal(bodymap)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, c.ingressURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Host = c.analysisHeader

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	analysis := &Analysis{}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if err = json.Unmarshal(b, analysis); err != nil {
		return "", err
	}

	if analysis.ID == "" {
		return "", errors.New("no analyses found")
	}

	return analysis.ID, nil
}

// Resource is an item that can have permissions attached to it in the
// permissions service.
type Resource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"resource_type"`
}

// Subject is an item that accesses resources contained in the permissions
// service.
type Subject struct {
	ID        string `json:"id"`
	SubjectID string `json:"subject_id"`
	SourceID  string `json:"subject_source_id"`
	Type      string `json:"subject_type"`
}

// Permission is an entry from the permissions service that tells what access
// a subject has to a resource.
type Permission struct {
	ID       string   `json:"id"`
	Level    string   `json:"permission_level"`
	Resource Resource `json:"resource"`
	Subject  Subject  `json:"subject"`
}

// PermissionList contains a list of permission returned by the permissions
// service.
type PermissionList struct {
	Permissions []Permission `json:"permissions"`
}

// IsAllowed will return true if the user is allowed to access the running app
// and false if they're not. An error might be returned as well. Access should
// be denied if an error is returned, even if the boolean return value is true.
func (c *CASProxy) IsAllowed(user, resource string) (bool, error) {
	bodymap := map[string]string{
		"subject":  user,
		"resource": resource,
	}

	body, err := json.Marshal(bodymap)
	if err != nil {
		return false, err
	}

	request, err := http.NewRequest(http.MethodPost, c.ingressURL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}

	request.Host = c.accessHeader

	client := &http.Client{}
	resp, err := client.Do(request)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	l := &PermissionList{
		Permissions: []Permission{},
	}

	if err = json.Unmarshal(b, l); err != nil {
		return false, err
	}

	if len(l.Permissions) > 0 {
		if l.Permissions[0].Level != "" {
			return true, nil
		}
	}

	return false, nil
}

// ValidateTicket will validate a CAS ticket against the configured CAS server.
func (c *CASProxy) ValidateTicket(w http.ResponseWriter, r *http.Request) {
	casURL, err := url.Parse(c.casBase)
	if err != nil {
		err = errors.Wrapf(err, "failed to parse CAS base URL %s", c.casBase)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Make sure the path in the CAS params is the same as the one that was
	// requested.
	svcURL, err := url.Parse(c.frontendURL)
	if err != nil {
		err = errors.Wrapf(err, "failed to parse the frontend URL %s", c.frontendURL)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Ensure that the service path and the query params are set to the incoming
	// request's values for those fields.
	svcURL.Path = r.URL.Path
	sq := r.URL.Query()
	sq.Del("ticket") // Remove the ticket from the service URL. Redirection loops occur otherwise.
	svcURL.RawQuery = sq.Encode()

	// The request URL for CAS ticket validation needs to have the service and
	// ticket in it.
	casURL.Path = path.Join(casURL.Path, c.casValidate)
	q := casURL.Query()
	q.Add("service", svcURL.String())
	q.Add("ticket", r.URL.Query().Get("ticket"))
	casURL.RawQuery = q.Encode()

	// Actually validate the ticket.
	resp, err := http.Get(casURL.String())
	if err != nil {
		err = errors.Wrap(err, "ticket validation error")
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	// If this happens then something went wrong on the CAS side of things. Doesn't
	// mean the ticket is invalid, just that the CAS server is in a state where
	// we can't trust the response.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err = errors.Wrapf(err, "ticket validation status code was %d", resp.StatusCode)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		err = errors.Wrap(err, "error reading body of CAS response")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// This is where the actual ticket validation happens. If the CAS server
	// returns 'no\n\n' in the body, then the validation was not successful. The
	// HTTP status code will be in the 200 range regardless of the validation
	// status.
	if bytes.Equal(b, []byte("no\n\n")) {
		err = fmt.Errorf("ticket validation response body was %s", b)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	fields := bytes.Fields(b)
	if len(fields) < 2 {
		err = errors.New("not enough fields in ticket validation response body")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	username := string(fields[1])

	// Store a session, hopefully to short-circuit the CAS redirect dance in later
	// requests. The max age of the cookie should be less than the lifetime of
	// the CAS ticket, which is around 10+ hours. This means that we'll be hitting
	// the CAS server fairly often. Adjust the max age to rate limit requests to
	// CAS.
	var s *sessions.Session
	s, err = c.sessionStore.Get(r, sessionName)
	if err != nil {
		err = errors.Wrap(err, "error getting session")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Values[sessionKey] = username
	s.Save(r, w)

	http.Redirect(w, r, svcURL.String(), http.StatusFound)
}

// ResetSessionExpiration should reset the session expiration time.
func (c *CASProxy) ResetSessionExpiration(w http.ResponseWriter, r *http.Request) error {
	session, err := c.sessionStore.Get(r, sessionName)
	if err != nil {
		return err
	}

	msg, ok := session.Values[sessionKey]
	if !ok {
		return errors.New("session value not found")
	}

	session.Values[sessionKey] = msg.(string)
	session.Save(r, w)
	return nil
}

// Session implements the mux.Matcher interface so that requests can be routed
// based on cookie existence.
func (c *CASProxy) Session(r *http.Request, m *mux.RouteMatch) bool {
	session, err := c.sessionStore.Get(r, sessionName)
	if err != nil {
		return true
	}

	msgraw, ok := session.Values[sessionKey]
	if !ok {
		return true
	}
	msg := msgraw.(string)
	if msg == "" {
		log.Infof("session value was empty instead of a username")
		return true
	}

	return false
}

// RedirectToCAS redirects the request to CAS, setting the service query
// parameter to the value in frontendURL.
func (c *CASProxy) RedirectToCAS(w http.ResponseWriter, r *http.Request) {
	fmt.Println("redirect to cas")
	casURL, err := url.Parse(c.casBase)
	if err != nil {
		err = errors.Wrapf(err, "failed to parse CAS base URL %s", c.casBase)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Make sure the path in the CAS params is the same as the one that was
	// requested.
	svcURL, err := url.Parse(c.frontendURL)
	if err != nil {
		err = errors.Wrapf(err, "failed to parse the frontend URL %s", c.frontendURL)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Make sure the serivce path and the query params are set to the incoming
	// requests values for those fields.
	svcURL.Path = r.URL.Path
	svcURL.RawQuery = r.URL.RawQuery

	//set the service query param in the casURL.
	q := casURL.Query()
	q.Add("service", svcURL.String())
	casURL.RawQuery = q.Encode()
	casURL.Path = path.Join(casURL.Path, "login")

	// perform the redirect
	http.Redirect(w, r, casURL.String(), http.StatusTemporaryRedirect)
}

// ReverseProxy returns a proxy that forwards requests to the configured
// backend URL. It can act as a http.Handler.
func (c *CASProxy) ReverseProxy() (*httputil.ReverseProxy, error) {
	backend, err := url.Parse(c.backendURL)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse %s", c.backendURL)
	}
	return httputil.NewSingleHostReverseProxy(backend), nil
}

// WSReverseProxy returns a proxy that forwards websocket request to the
// configured backend URL. It can act as a http.Handler.
func (c *CASProxy) WSReverseProxy() (*wsutil.ReverseProxy, error) {
	w, err := url.Parse(c.wsbackendURL)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse the websocket backend URL %s", c.wsbackendURL)
	}
	return wsutil.NewSingleHostReverseProxy(w), nil
}

// isWebsocket returns true if the connection is a websocket request. Adapted
// from the code at https://groups.google.com/d/msg/golang-nuts/KBx9pDlvFOc/0tR1gBRfFVMJ.
func (c *CASProxy) isWebsocket(r *http.Request) bool {
	connectionHeader := ""
	allHeaders := r.Header["Connection"]
	if len(allHeaders) > 0 {
		connectionHeader = allHeaders[0]
	}

	upgrade := false
	if strings.Contains(strings.ToLower(connectionHeader), "upgrade") {
		if len(r.Header["Upgrade"]) > 0 {
			upgrade = (strings.ToLower(r.Header["Upgrade"][0]) == "websocket")
		}
	}
	return upgrade
}

func (c *CASProxy) backendIsReady(backendURL string) (bool, error) {
	resp, err := http.Get(backendURL)
	if err != nil {
		return false, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 399 {
		return true, nil
	}
	return false, nil

}

// URLIsReady will write out a JSON-encoded response in the format
// {"ready":boolean}, telling whether or not the underlying application is ready
// for business yet.
func (c *CASProxy) URLIsReady(w http.ResponseWriter, r *http.Request) {
	ready, err := c.backendIsReady(c.backendURL)
	if err != nil {
		log.Error(err)
	}

	data := map[string]bool{
		"ready": ready,
	}

	body, err := json.Marshal(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if ready {
		fmt.Fprintf(w, string(body))
	} else {
		http.Error(w, string(body), http.StatusNotAcceptable)
	}
}

// Proxy returns a handler that can support both websockets and http requests.
func (c *CASProxy) Proxy() (http.Handler, error) {
	ws, err := c.WSReverseProxy()
	if err != nil {
		return nil, err
	}

	rp, err := c.ReverseProxy()
	if err != nil {
		return nil, err
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//Get the username from the cookie
		session, err := c.sessionStore.Get(r, sessionName)
		if err != nil {
			err = errors.Wrap(err, "failed to get session")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		username := session.Values[sessionKey].(string)
		if username == "" {
			err = errors.Wrap(err, "username was empty")
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		// Check to make sure the user can access the resource.
		allowed, err := c.IsAllowed(username, c.resourceName)
		if !allowed || err != nil {
			if err != nil {
				err = errors.Wrap(err, "access denied")
			} else {
				err = errors.New("access denied")
			}
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		log.Printf("%+v\n", r.Header)

		if err = c.ResetSessionExpiration(w, r); err != nil {
			err = errors.Wrap(err, "error resetting session expiration")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if c.isWebsocket(r) {
			ws.ServeHTTP(w, r)
			return
		}
		rp.ServeHTTP(w, r)
	}), nil
}

type originFlags []string

func (o *originFlags) String() string {
	return strings.Join([]string(*o), ",")
}

func (o *originFlags) Set(s string) error {
	parts := strings.Split(s, ",")
	*o = append(*o, parts...)
	return nil
}

func main() {
	var (
		corsOrigins    originFlags
		backendURL     = flag.String("backend-url", "http://localhost:60000", "The hostname and port to proxy requests to.")
		wsbackendURL   = flag.String("ws-backend-url", "", "The backend URL for the handling websocket requests. Defaults to the value of --backend-url with a scheme of ws://")
		frontendURL    = flag.String("frontend-url", "", "The URL for the frontend server. Might be different from the hostname and listen port.")
		listenAddr     = flag.String("listen-addr", "0.0.0.0:8080", "The listen port number.")
		casBase        = flag.String("cas-base-url", "", "The base URL to the CAS host.")
		casValidate    = flag.String("cas-validate", "validate", "The CAS URL endpoint for validating tickets.")
		maxAge         = flag.Int("max-age", 0, "The idle timeout for session, in seconds.")
		sslCert        = flag.String("ssl-cert", "", "Path to the SSL .crt file.")
		sslKey         = flag.String("ssl-key", "", "Path to the SSL .key file.")
		ingressURL     = flag.String("ingress-url", "", "The URL to the cluster ingress.")
		analysisHeader = flag.String("analysis-header", "get-analysis-id", "The Host header for the ingress service that gets the analysis ID.")
		accessHeader   = flag.String("access-header", "check-resource-access", "The Host header for the ingress service that checks analysis access.")
		externalID     = flag.String("external-id", "", "The external ID to pass to the apps service when looking up the analysis ID.")
	)

	flag.Var(&corsOrigins, "allowed-origins", "List of allowed origins, separated by commas.")
	flag.Parse()

	if *casBase == "" {
		log.Fatal("--cas-base-url must be set.")
	}

	if *frontendURL == "" {
		log.Fatal("--frontend-url must be set.")
	}

	useSSL := false
	if *sslCert != "" || *sslKey != "" {
		if *sslCert == "" {
			log.Fatal("--ssl-cert is required with --ssl-key.")
		}

		if *sslKey == "" {
			log.Fatal("--ssl-key is required with --ssl-cert.")
		}
		useSSL = true
	}

	if len(corsOrigins) < 1 {
		corsOrigins = originFlags{"*.cyverse.run", "*.cyverse.org", "*.cyverse.run:4343", "cyverse.run", "cyverse.run:4343"}
	}

	if *wsbackendURL == "" {
		w, err := url.Parse(*backendURL)
		if err != nil {
			log.Fatal(err)
		}
		w.Scheme = "ws"
		*wsbackendURL = w.String()
	}

	if *ingressURL == "" {
		log.Fatal("--ingress-url must be set.")
	}

	if *externalID == "" {
		log.Fatal("--external-id must be set.")
	}

	log.Infof("backend URL is %s", *backendURL)
	log.Infof("websocket backend URL is %s", *wsbackendURL)
	log.Infof("frontend URL is %s", *frontendURL)
	log.Infof("listen address is %s", *listenAddr)
	log.Infof("CAS base URL is %s", *casBase)
	log.Infof("CAS ticket validator endpoint is %s", *casValidate)

	for _, c := range corsOrigins {
		log.Infof("Origin: %s\n", c)
	}

	authkey := make([]byte, 64)
	_, err := rand.Read(authkey)
	if err != nil {
		log.Fatal(err)
	}

	sessionStore := sessions.NewCookieStore(authkey)
	sessionStore.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   *maxAge,
		HttpOnly: true,
	}

	p := &CASProxy{
		casBase:        *casBase,
		casValidate:    *casValidate,
		frontendURL:    *frontendURL,
		backendURL:     *backendURL,
		wsbackendURL:   *wsbackendURL,
		ingressURL:     *ingressURL,
		accessHeader:   *accessHeader,
		analysisHeader: *analysisHeader,
		sessionStore:   sessionStore,
	}

	resourceName, err := p.getResourceName(*externalID)
	if err != nil {
		log.Fatal(err)
	}
	p.resourceName = resourceName

	proxy, err := p.Proxy()
	if err != nil {
		log.Fatal(err)
	}

	r := mux.NewRouter()

	// If the query contains a ticket in the query params, then it needs to be
	// validated.
	r.PathPrefix("/url-ready").HandlerFunc(p.URLIsReady)
	r.PathPrefix("/").Queries("ticket", "").Handler(http.HandlerFunc(p.ValidateTicket))
	r.PathPrefix("/").MatcherFunc(p.Session).Handler(http.HandlerFunc(p.RedirectToCAS))
	r.PathPrefix("/").Handler(proxy)

	c := cors.New(cors.Options{
		AllowedOrigins:   corsOrigins,
		AllowCredentials: true,
	})

	server := &http.Server{
		Handler: c.Handler(r),
		Addr:    *listenAddr,
	}
	if useSSL {
		err = server.ListenAndServeTLS(*sslCert, *sslKey)
	} else {
		err = server.ListenAndServe()
	}
	log.Fatal(err)

}
