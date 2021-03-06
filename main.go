package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/go-chi/chi/v5"
	"github.com/go-co-op/gocron"
	_ "github.com/jackc/pgx/v4/stdlib"
)

type StartRequest struct {
	Attributes   []string `json:"attributes"`
	Continuation string   `json:"continuation"`
	AttributeURL *string  `json:"attr_url"`
}

type StartResponse struct {
	ClientURL string `json:"client_url"`
}

// Start ID Contact authentication session
func (c *Configuration) startSession(w http.ResponseWriter, r *http.Request) {
	log.Debug("Starting session")
	// Extract request
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}
	var request StartRequest
	err = json.Unmarshal(body, &request)
	if err != nil {
		w.WriteHeader(400)
		log.Warn(err)
		return
	}

	// Validate requested attributes
	for _, attribute := range request.Attributes {
		_, ok := c.AttributeMapping[attribute]
		if !ok {
			w.WriteHeader(400)
			log.Warn(err)
			return
		}
	}

	// Create a new session in the database
	encodedAttributes, err := json.Marshal(request.Attributes)
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}
	session, err := c.SessionManager.NewSession(string(encodedAttributes), request.Continuation, request.AttributeURL)
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}

	// And instruct the core appropriately
	clientURL := *c.ServerURL
	clientURL.Path = path.Join(clientURL.Path, "session", session.id)
	response, err := json.Marshal(StartResponse{ClientURL: clientURL.String()})
	w.WriteHeader(200)
	w.Write(response)
}

type AuthResult struct {
	status     string
	attributes map[string]string
}

// Handle an actual end-user login
func (c *Configuration) doLogin(w http.ResponseWriter, r *http.Request) {
	// Fetch corresponding ID Contact session
	id := chi.URLParam(r, "sessionid")
	session, err := c.SessionManager.GetSession(id)
	if err != nil {
		w.WriteHeader(400)
		log.Warn(err)
		return
	}

	var attributes []string
	err = json.Unmarshal([]byte(session.attributes), &attributes)
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}

	authnContextClass := samlsp.AttributeFromContext(r.Context(), "AuthnContextClassRef")
	if !CompareAuthnContextClass(c.AuthnContextClassRef, authnContextClass) {
		w.WriteHeader(500)
		log.Error("AuthnContextClass too low", authnContextClass)
		return
	}

	// Extract attributes from BRP:
	samlsession := samlsp.SessionFromContext(r.Context()).(*SamlSession)
	bsn := samlsession.attributes.Get("NameID")
	if bsn[:9] != "s00000000" {
		w.WriteHeader(500)
		log.Error("Unexpected sectoral code", bsn[:9])
		return
	}
	altbsn, ok := c.TestBSNMapping[bsn[10:]]
	if ok {
		bsn = "s00000000:" + altbsn
	}
	attributeResult, err := GetBRPAttributes(c.BRPServer, bsn[10:], c.AttributeMapping, c.Client, c.CaCerts)
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}

	// Encode attributes
	attributesJSON, err := json.Marshal(attributeResult)
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}

	// And a confirmation screen JWT
	logoutURL := *c.ServerURL
	logoutURL.Path = path.Join(logoutURL.Path, "update", samlsession.logoutid)
	logoutQuery := logoutURL.Query()
	logoutQuery.Set("type", "logout")
	logoutURL.RawQuery = logoutQuery.Encode()
	confirmURL := *c.ServerURL
	confirmURL.Path = path.Join(confirmURL.Path, "confirm", id)
	attributeURL := *c.ServerURL
	attributeURL.Path = path.Join(attributeURL.Path, "attributes", id)
	confirmationToken, err := buildConfirmationJWT(attributeURL.String(), logoutURL.String(), confirmURL.String(), c.ConfirmationSigningKey)
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}

	// Store the information needed for confirmation
	err = c.SamlSessionManager.SetIDContactSession(samlsession, id, string(attributesJSON))
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}

	// And redirect the user to the confirmation screen
	confirmationScreenUrl := *c.ConfirmationURL
	confirmQuery := confirmationScreenUrl.Query()
	confirmQuery.Set("data", string(confirmationToken))
	confirmationScreenUrl.RawQuery = confirmQuery.Encode()
	http.Redirect(w, r, confirmationScreenUrl.String(), 302)
}

func (c *Configuration) getAttributes(w http.ResponseWriter, r *http.Request) {
	samlsession := samlsp.SessionFromContext(r.Context()).(*SamlSession)
	url_sessionid := chi.URLParam(r, "sessionid")

	// this is activity on the session, so mark it
	c.SamlSessionManager.MarkActive(samlsession.logoutid)

	// Get jwt and session id
	sessionid, attributeJSON, err := c.SamlSessionManager.GetIDContactSession(samlsession)
	if err == samlsp.ErrNoSession {
		w.WriteHeader(400)
		log.Warn(err)
		return
	}
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}
	if url_sessionid != sessionid {
		w.WriteHeader(400)
		log.Warn("Confirmation received from user for session that is not it's most current")
		return
	}

	// check id contact session exists
	_, err = c.SessionManager.GetSession(sessionid)
	if err != nil {
		w.WriteHeader(400)
		log.Warn(err)
		return
	}

	// Return attributes
	_, err = w.Write([]byte(attributeJSON))
	if err != nil {
		// there is no good way to recover the http session here, so just log and exit
		log.Error(err)
		return
	}
}

func (c *Configuration) doConfirm(w http.ResponseWriter, r *http.Request) {
	samlsession := samlsp.SessionFromContext(r.Context()).(*SamlSession)
	url_sessionid := chi.URLParam(r, "sessionid")

	// this is activity on the session, so mark it
	c.SamlSessionManager.MarkActive(samlsession.logoutid)

	// Get jwt and session id
	sessionid, attributesJSON, err := c.SamlSessionManager.GetIDContactSession(samlsession)
	if err == samlsp.ErrNoSession {
		w.WriteHeader(400)
		log.Warn(err)
		return
	}
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}
	if url_sessionid != sessionid {
		w.WriteHeader(400)
		log.Warn("Confirmation received from user for session that is not it's most current")
		return
	}

	session, err := c.SessionManager.GetSession(sessionid)
	if err != nil {
		w.WriteHeader(400)
		log.Warn(err)
		return
	}

	// Construct authentication result JWT
	var attributes map[string]string
	err = json.Unmarshal([]byte(attributesJSON), &attributes)
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}
	logoutUrl := *c.InternalURL
	logoutUrl.Path = path.Join(logoutUrl.Path, "update", samlsession.logoutid)
	authToken, err := buildAttributeJWT(attributes, logoutUrl.String(), c.JwtSigningKey, c.JwtEncryptionKey)
	if err != nil {
		w.WriteHeader(500)
		log.Error(err)
		return
	}

	// And deliver it appropriately
	if session.attributeURL != nil {
		response, err := http.Post(*session.attributeURL, "application/jwt", bytes.NewReader([]byte(authToken)))
		if err != nil {
			// Just log
			log.Error(err)
		} else {
			defer response.Body.Close()
			if response.StatusCode >= 300 {
				log.Errorf("attribute url failed (%d)\n", response.StatusCode)
			}
		}
		http.Redirect(w, r, session.continuation, 302)
	} else {
		redirectURL, err := url.Parse(session.continuation)
		if err != nil {
			w.WriteHeader(500)
			log.Error(err)
			return
		}
		redirectQuery := redirectURL.Query()
		redirectQuery.Set("result", string(authToken))
		redirectURL.RawQuery = redirectQuery.Encode()
		http.Redirect(w, r, redirectURL.String(), 302)
	}
}

// Handle update on session from communication plugin.
func (c *Configuration) SessionUpdate(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		log.Warn(err)
		w.WriteHeader(400)
		return
	}

	updateType := r.FormValue("type")
	if updateType == "logout" {
		// Handle logout request
		err = c.SamlSessionManager.Logout(chi.URLParam(r, "logoutid"))
		if err != nil {
			log.Error("Logout failed: ", err)
			// Note, this error shouldn't be propagated to remote
		}
	} else if updateType == "user_active" {
		err = c.SamlSessionManager.MarkActive(chi.URLParam(r, "logoutid"))
		if err != nil {
			log.Error("Activity marking failed: ", err)
			// Note, this error shouldn't be propagated to remote
		}
	} else {
		log.Warn("Unrecognized update type ", updateType)
	}

	w.WriteHeader(204)
}

func (c *Configuration) BuildHandler() http.Handler {
	// Setup SAML plugin
	idpMetadata, err := samlsp.FetchMetadata(context.Background(), http.DefaultClient,
		*c.IdpMetadataURL)
	if err != nil {
		log.Fatal("Failed to download IdP metadata: ", err)
	}

	samlSP, err := samlsp.New(samlsp.Options{
		EntityID:             c.EntityID,
		URL:                  *c.ServerURL,
		Key:                  c.SamlKeyPair.PrivateKey.(*rsa.PrivateKey),
		Certificate:          c.SamlKeyPair.Leaf,
		TLSClientCertificate: &c.SamlKeyPair,
		IDPMetadata:          idpMetadata,
		SignRequest:          true,
		UseArtifactResponse:  true,
		RequestedAuthnContext: &saml.RequestedAuthnContext{
			Comparison:           "minimum",
			AuthnContextClassRef: c.AuthnContextClassRef,
		},
	})
	samlSP.Session = &samlsp.CookieSessionProvider{
		Name:     "samlsession",
		Domain:   c.ServerURL.Host,
		HTTPOnly: true,
		Secure:   c.ServerURL.Scheme == "https",
		MaxAge:   60 * time.Minute,
		Codec:    c.SamlSessionManager,
	}

	// Construct router
	r := chi.NewRouter()
	if c.SentryDSN != "" {
		sentryMiddleware := sentryhttp.New(sentryhttp.Options{})
		r.Use(sentryMiddleware.Handle)
	}

	r.Group(func(r chi.Router) {
		r.Use(samlSP.RequireAccount)
		r.Get("/session/{sessionid}", c.doLogin)
		r.Get("/attributes/{sessionid}", c.getAttributes)
		r.Post("/confirm/{sessionid}", c.doConfirm)
	})

	r.Post("/start_authentication", c.startSession)
	r.Post("/update/{logoutid}", c.SessionUpdate)
	r.Mount("/saml/", samlSP)

	return r
}

var release string

type SentryLogHook struct{}

func (t *SentryLogHook) Levels() []log.Level {
	return []log.Level{
		log.PanicLevel,
		log.FatalLevel,
		log.ErrorLevel,
	}
}

func (t *SentryLogHook) Fire(event *log.Entry) error {
	sentry_event := sentry.Event{
		Message:  event.Message,
		Contexts: event.Data,
	}
	if event.Level == log.ErrorLevel {
		sentry_event.Level = sentry.LevelError
	} else {
		sentry_event.Level = sentry.LevelFatal
	}
	sentry.CaptureEvent(&sentry_event)
	return nil
}

func main() {
	configuration := ParseConfiguration()
	if configuration.SentryDSN != "" {
		// Setup sentry
		err := sentry.Init(sentry.ClientOptions{
			Dsn:         configuration.SentryDSN,
			Release:     release,
			ServerName:  "auth-digid",
			Environment: os.Getenv("ENVIRONMENT"),
		})
		if err != nil {
			log.Fatal("Error starting sentry: ", err)
		}
		defer sentry.Recover()

		// And hook into logging
		log.AddHook(&SentryLogHook{})
	}

	s := gocron.NewScheduler(time.UTC)
	s.Every("1m").Do(func() {
		configuration.SamlSessionManager.Cleanup()
		configuration.SessionManager.Cleanup()
	})
	s.StartAsync()

	http.Handle("/", configuration.BuildHandler())
	http.ListenAndServe(":8000", nil)
}
