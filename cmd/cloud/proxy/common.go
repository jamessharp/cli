package proxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid/v3"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/pkg/errors"
	"github.com/smallstep/truststore"
	"github.com/spf13/cobra"
	"github.com/square/go-jose/v3"
	"github.com/square/go-jose/v3/jwt"
	"github.com/tidwall/gjson"
	"github.com/urfave/negroni"

	"github.com/ory/cli/cmd/cloud/remote"
	"github.com/ory/cli/x"
	"github.com/ory/graceful"
	"github.com/ory/herodot"
	"github.com/ory/x/httpx"
	"github.com/ory/x/jwksx"
	"github.com/ory/x/logrusx"
	"github.com/ory/x/tlsx"
	"github.com/ory/x/urlx"
)

const (
	PortFlag          = "port"
	NoCertInstallFlag = "dont-install-cert"
	NoOpenFlag        = "no-open"
)

type config struct {
	port            int
	noCert          bool
	noOpen          bool
	apiEndpoint     string
	consoleEndpoint string
	hostPort        string
	isLocal         bool
	upstream        string
}

func portFromEnv() int {
	var port int64 = 4000
	if p, _ := strconv.ParseInt(os.Getenv("PORT"), 10, 64); p != 0 {
		port = p
	}
	return int(port)
}

func run(cmd *cobra.Command, conf *config) error {
	upstream, err := url.ParseRequestURI(conf.upstream)
	if err != nil {
		return errors.Wrap(err, "unable to parse upstream URL")
	}

	c, cleanup, err := createTLSCertificate(conf)
	if err != nil {
		return err
	}

	l := logrusx.New("ory/proxy", x.BuildVersion)

	handler := httputil.NewSingleHostReverseProxy(upstream)
	writer := herodot.NewJSONWriter(l)

	mw := negroni.New()
	// mw.Use(reqlog.NewMiddlewareFromLogger(l, "ory"))

	signer, key, err := newSigner(l)
	if err != nil {
		return errors.WithStack(err)
	}

	endpoint, err := getEndpointURL(cmd, conf)
	if err != nil {
		return errors.WithStack(err)
	}

	mw.UseFunc(func(w http.ResponseWriter, r *http.Request, n http.HandlerFunc) {
		// Disable HSTS because it is very annoying to use in localhost.
		w.Header().Set("Strict-Transport-Security", "max-age=0;")
		n(w, r)
	})

	mw.UseFunc(checkOry(conf, writer, l, key, signer, endpoint)) // This must be the last method before the handler
	mw.UseHandler(handler)

	addr := fmt.Sprintf(":%d", conf.port)
	server := graceful.WithDefaults(&http.Server{
		Addr:      addr,
		Handler:   mw,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{*c}},
	})

	l.Printf("Starting the https reverse proxy on: %s", server.Addr)
	proxyUrl := fmt.Sprintf("https://%s", conf.hostPort)
	l.Printf(`To access your application through the Ory Proxy, open:

	%s`, proxyUrl)
	if !conf.noOpen {
		if err := exec.Command("open", proxyUrl).Run(); err != nil {
			l.WithError(err).Warn("Unable to automatically open the proxy URL in your browser. Please open it manually!")
		}
	}

	if err := graceful.Graceful(func() error {
		return server.ListenAndServeTLS("", "")
	}, func(ctx context.Context) error {
		l.Println("http reverse proxy was shutdown gracefully")
		if err := server.Shutdown(ctx); err != nil {
			return err
		}

		return cleanup()
	}); err != nil {
		l.Fatalln("Failed to gracefully shutdown https reverse proxy")
	}

	return nil
}

func newSigner(l *logrusx.Logger) (jose.Signer, *jose.JSONWebKeySet, error) {
	l.WithField("started_at", time.Now()).Info("")
	key, err := jwksx.GenerateSigningKeys(
		uuid.Must(uuid.NewV4()).String(),
		"ES256",
		0,
	)
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to generate JSON Web Key")
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key.Keys[0].Key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to create signer")
	}
	l.WithField("completed_at", time.Now()).Info("ES256 JSON Web Key generation completed.")
	return sig, key, nil
}

func initUrl(method string, conf *config) string {
	return fmt.Sprintf("/.ory/api/kratos/public/self-service/%s/browser?return_to=%s", method, "https://"+conf.hostPort)
}

func checkOry(conf *config, writer herodot.Writer, l *logrusx.Logger, keys *jose.JSONWebKeySet, sig jose.Signer, endpoint *url.URL) func(http.ResponseWriter, *http.Request, http.HandlerFunc) {
	hc := httpx.NewResilientClient(httpx.ResilientClientWithMaxRetry(5), httpx.ResilientClientWithMaxRetryWait(time.Millisecond*5), httpx.ResilientClientWithConnectionTimeout(time.Second*2))

	var publicKeys jose.JSONWebKeySet
	for _, key := range keys.Keys {
		publicKeys.Keys = append(publicKeys.Keys, key.Public())
	}

	oryUpstream := httputil.NewSingleHostReverseProxy(endpoint)
	oryUpstream.ModifyResponse = func(res *http.Response) error {
		if !strings.EqualFold(res.Request.Host, endpoint.Host) {
			// not ory
			return nil
		}

		redir, _ := res.Location()
		if redir != nil {
			if strings.EqualFold(redir.Host, endpoint.Host) {
				redir.Host = conf.hostPort
				redir.Path = "/.ory" + strings.TrimPrefix(redir.Path, "/.ory")
				res.Header.Set("Location", redir.String())
			}
		}

		cookies := res.Cookies()
		res.Header.Del("Set-Cookie")
		for _, c := range cookies {
			if !strings.EqualFold(c.Domain, endpoint.Hostname()) {
				continue
			}
			c.Domain = ""
			res.Header.Add("Set-Cookie", c.String())
		}

		return nil
	}

	return func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
		if r.URL.Path == "/.ory/jwks.json" {
			writer.Write(w, r, publicKeys)
			return
		}

		if r.URL.Path == "/.ory/login" {
			writer.Write(w, r, publicKeys)
			return
		}

		switch r.URL.Path {
		case "/.ory/jwks.json":
			writer.Write(w, r, publicKeys)
			return
		case "/.ory/init/login":
			http.Redirect(w, r, initUrl("login", conf), http.StatusSeeOther)
			return
		case "/.ory/init/registration":
			http.Redirect(w, r, initUrl("registration", conf), http.StatusSeeOther)
			return
		case "/.ory/init/recovery":
			http.Redirect(w, r, initUrl("recovery", conf), http.StatusSeeOther)
			return
		case "/.ory/init/verification":
			http.Redirect(w, r, initUrl("verification", conf), http.StatusSeeOther)
			return
		case "/.ory/init/settings":
			http.Redirect(w, r, initUrl("settings", conf), http.StatusSeeOther)
			return
		}

		// We proxy ory things
		if strings.HasPrefix(r.URL.Path, "/.ory") {
			r.URL.Path = strings.ReplaceAll(r.URL.Path, "/.ory/", "/")
			r.Host = endpoint.Host
			q := r.URL.Query()
			q.Set("isProxy", "true")
			r.URL.RawQuery = q.Encode()

			l.WithRequest(r).
				WithField("forwarding_path", r.URL.String()).
				WithField("forwarding_host", r.Host).
				Debug("Forwarding request to Ory.")
			oryUpstream.ServeHTTP(w, r)
			return
		}

		session, err := checkSession(hc, r, endpoint)
		r.Header.Del("Authorization")
		if err != nil || !gjson.GetBytes(session, "active").Bool() {
			next(w, r)
			return
		}

		now := time.Now().UTC()
		raw, err := jwt.Signed(sig).Claims(&jwt.Claims{
			Issuer:    endpoint.String(),
			Subject:   gjson.GetBytes(session, "identity.id").String(),
			Expiry:    jwt.NewNumericDate(now.Add(time.Minute)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        uuid.Must(uuid.NewV4()).String(),
		}).Claims(map[string]interface{}{"session": session}).CompactSerialize()
		if err != nil {
			writer.WriteError(w, r, err)
			return
		}

		r.Header.Set("Authorization", "Bearer "+raw)
		next(w, r)
	}
}

func checkSession(c *retryablehttp.Client, r *http.Request, target *url.URL) (json.RawMessage, error) {
	target = urlx.Copy(target)
	target.Path = filepath.Join(target.Path, "api", "kratos", "public", "sessions", "whoami")
	req, err := retryablehttp.NewRequest("GET", target.String(), nil)
	if err != nil {
		return nil, errors.WithStack(herodot.ErrInternalServerError)
	}

	req.Header.Set("Cookie", r.Header.Get("Cookie"))
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	req.Header.Set("X-Session-Token", r.Header.Get("X-Session-Token"))
	req.Header.Set("X-Request-Id", r.Header.Get("X-Request-Id"))
	req.Header.Set("Accept", "application/json")

	res, err := c.Do(req)
	if err != nil {
		return nil, errors.WithStack(herodot.ErrInternalServerError.WithReasonf("Unable to call session checker: %s", err).WithWrap(err))
	}
	defer res.Body.Close()

	var body json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, errors.WithStack(herodot.ErrInternalServerError.WithReasonf("Unable to decode session to JSON: %s", err).WithWrap(err))
	}

	return body, nil
}

func getEndpointURL(cmd *cobra.Command, conf *config) (*url.URL, error) {
	if target, ok := cmd.Context().Value(remote.FlagAPIEndpoint).(*url.URL); ok {
		return target, nil
	}
	upstream, err := url.ParseRequestURI(conf.apiEndpoint)
	if err != nil {
		return nil, err
	}
	project, err := remote.GetProjectSlug(conf.consoleEndpoint)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	upstream.Host = fmt.Sprintf("%s.projects.%s", project, upstream.Host)

	return upstream, nil
}

func createTLSCertificate(conf *config) (*tls.Certificate, func() error, error) {
	key, err := rsa.GenerateKey(rand.Reader, 4096)

	c, err := tlsx.CreateSelfSignedCertificate(key)
	if err != nil {
		return nil, nil, err
	}

	block, err := tlsx.PEMBlockForKey(key)
	if err != nil {
		return nil, nil, err
	}

	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	pemKey := pem.EncodeToMemory(block)
	cert, err := tls.X509KeyPair(pemCert, pemKey)
	if err != nil {
		return nil, nil, err
	}

	const passwordMessage = "To modify your operating system certificate store, you might might be prompted for your password now:"

	if conf.noCert {
		return &cert, func() error {
			return nil
		}, nil
	}

	_, _ = fmt.Fprintln(os.Stdout, "Trying to install temporary TLS (HTTPS) certificate for localhost on your operating system. This allows to access the proxy using HTTPS.")
	_, _ = fmt.Fprintln(os.Stdout, passwordMessage)
	opts := []truststore.Option{
		truststore.WithFirefox(),
		truststore.WithJava(),
	}

	if err := truststore.Install(c, opts...); err != nil {
		return nil, nil, err
	}

	return &cert, func() error {
		_, _ = fmt.Fprintln(os.Stdout, passwordMessage)
		return truststore.Uninstall(c, opts...)
	}, nil
}