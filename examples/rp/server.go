package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jws"

	"github.com/zachmann/go-oidfed/examples/rp/pkce"
	"github.com/zachmann/go-oidfed/internal/utils"
	"github.com/zachmann/go-oidfed/pkg"
	"github.com/zachmann/go-oidfed/pkg/apimodel"
)

const loginHtml = `<!DOCTYPE html>
<html>
<head>
<link rel="stylesheet" href="https://cdn.simplecss.org/simple.min.css">
</head>
<body>
<h1>Dummy oidfed RP</h1>
<h2>%s</h2>
%s
<h4>Choose an OP from the supported federations to login</h4>
<form action="login">
  <select name="op" id="op">
	<option value="/" selected disabled>Choose OP...</option>
	%s
  </select>
  <input type="submit" value="Login">
</form>
</body>
</html>`

const userHtml = `<!DOCTYPE html>
<html>
<head>
<link rel="stylesheet" href="https://cdn.simplecss.org/simple.min.css">
</head>
<body>
<h3>Hello %s@%s</h3>
<a href="/">Back to Login</a>
</body>
</html>`

const errorHtml = `<!DOCTYPE html>
<html>
<head>
<link rel="stylesheet" href="https://cdn.simplecss.org/simple.min.css">
</head>
<body>
<h3>Error %s</h3>
<p>%s</p>
<a href="/">Back to Login</a>
</body>
</html>`

func handleHome(w http.ResponseWriter, r *http.Request) {
	const opOptionFmt = `<option value="%s">%s</option>`
	var options string
	filters := []pkg.EntityCollectionFilter{}
	if conf.OnlyAutomaticOPs {
		filters = append(
			filters, pkg.EntityCollectionFilterOPSupportsAutomaticRegistration(conf.TrustAnchors.EntityIDs()),
		)
	}
	allOPs := make(map[string]*pkg.CollectedEntity)
	for _, ta := range conf.TrustAnchors {
		var collector pkg.EntityCollector
		if conf.UseEntityCollectionEndpoint {
			collector = pkg.SmartRemoteEntityCollector{TrustAnchors: conf.TrustAnchors.EntityIDs()}
		} else {
			collector = &pkg.SimpleEntityCollector{}
		}
		ops := pkg.FilterableVerifiedChainsEntityCollector{
			Collector: collector,
			Filters:   filters,
		}.CollectEntities(
			apimodel.EntityCollectionRequest{
				TrustAnchor: ta.EntityID,
				EntityTypes: []string{"openid_provider"},
			},
		)
		for _, op := range ops {
			allOPs[op.EntityID] = op
		}
	}
	for _, op := range allOPs {
		options += fmt.Sprintf(
			opOptionFmt, op.EntityID, utils.FirstNonEmpty(
				op.DisplayNames["openid_provider"],
				op.DisplayNames["federation_entity"],
				op.EntityID,
			),
		)
	}
	var img string
	if conf.LogoURI != "" {
		img = fmt.Sprintf(`<img src="%s" alt="%s" />`, conf.LogoURI, "Logo")
	}
	_, _ = io.WriteString(w, fmt.Sprintf(loginHtml, conf.ClientName, img, options))
}

type stateData struct {
	codeChallange *pkce.PKCE
	issuer        string
}

var stateDB map[string]stateData

func errorPage(error, message string) string {
	return fmt.Sprintf(errorHtml, error, message)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if authBuilder == nil {
		authBuilder = pkg.NewRequestObjectProducer(conf.EntityID, getKey(oidcSigningKeyName), jwa.ES512(), 60)
	}
	op := r.URL.Query().Get("op")
	state := randASCIIString(32)
	pkceChallenge := pkce.NewS256PKCE(randASCIIString(64))
	stateDB[state] = stateData{
		codeChallange: pkceChallenge,
		issuer:        op,
	}
	challenge, err := pkceChallenge.Challenge()
	if err != nil {
		log.Fatal(err)
	}

	params := url.Values{}
	params.Set("nonce", randASCIIString(32))
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", pkce.TransformationS256.String())
	params.Set("prompt", "consent")

	authURL, err := fedLeaf().GetAuthorizationURL(op, redirectURI, state, "openid", params)
	if err != nil {
		log.Fatal(err)
	}
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

func handleCodeExchange(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	e := r.URL.Query().Get("error")
	errorDescription := r.URL.Query().Get("error_description")
	if e != "" {
		w.WriteHeader(444)
		_, _ = io.WriteString(w, errorPage(e, errorDescription))
		return
	}
	stateInfo, found := stateDB[state]
	if !found {
		w.WriteHeader(444)
		_, _ = io.WriteString(w, errorPage("state mismatch", ""))
		return
	}
	params := url.Values{}
	params.Set("code_verifier", stateInfo.codeChallange.Verifier())

	tokenRes, errRes, err := fedLeaf().CodeExchange(stateInfo.issuer, code, redirectURI, params)
	if err != nil {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, errorPage("internal error", err.Error()))
		return
	}
	if errRes != nil {
		w.WriteHeader(444)
		_, _ = io.WriteString(w, errorPage(errRes.Error, errRes.ErrorDescription))
		return
	}

	msg, err := jws.ParseString(tokenRes.IDToken)
	if err != nil {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, errorPage("internal error", err.Error()))
		return
	}
	delete(stateDB, state)
	msgData := map[string]any{}
	err = json.Unmarshal(msg.Payload(), &msgData)
	if err != nil {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, errorPage("internal error", err.Error()))
		return
	}

	w.WriteHeader(200)
	_, _ = io.WriteString(w, fmt.Sprintf(userHtml, msgData["sub"], msgData["iss"]))
}

var authBuilder *pkg.RequestObjectProducer
var _fedLeaf *pkg.FederationLeaf

func fedLeaf() *pkg.FederationLeaf {
	if _fedLeaf == nil {
		metadata := &pkg.Metadata{
			RelyingParty: &pkg.OpenIDRelyingPartyMetadata{
				// Scope:                   "openid",
				RedirectURIS:            []string{redirectURI},
				ResponseTypes:           []string{"code"},
				GrantTypes:              []string{"authorization_code"},
				ApplicationType:         "web",
				ClientName:              conf.ClientName,
				LogoURI:                 conf.LogoURI,
				JWKS:                    getJWKS(oidcSigningKeyName),
				OrganizationName:        conf.OrganisationName,
				ClientRegistrationTypes: []string{"automatic"},
			},
			FederationEntity: &pkg.FederationEntityMetadata{
				OrganizationName: conf.OrganisationName,
				LogoURI:          conf.LogoURI,
			},
		}
		var err error
		_fedLeaf, err = pkg.NewFederationLeaf(
			conf.EntityID, conf.AuthorityHints, conf.TrustAnchors, metadata,
			pkg.NewEntityStatementSigner(
				getKey(fedSigningKeyName),
				jwa.ES512(),
			), 86400, getKey(oidcSigningKeyName), jwa.ES512(),
		)
		if err != nil {
			log.Fatal(err)
		}
	}
	_fedLeaf.TrustMarks = conf.TrustMarks
	return _fedLeaf
}

var redirectURI string

func handleEntityConfiguration(w http.ResponseWriter, r *http.Request) {
	var err error

	jwt, err := fedLeaf().EntityConfigurationJWT()
	if err != nil {
		log.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/entity-statement+jwt")
	_, _ = w.Write(jwt)
}

func initServer() {
	redirectURI = fmt.Sprintf("%s/%s", conf.EntityID, "redirect")
	stateDB = make(map[string]stateData)

	http.HandleFunc("/", handleHome)
	http.HandleFunc("/login", handleLogin)
	http.HandleFunc("/redirect", handleCodeExchange)
	http.HandleFunc("/.well-known/openid-federation", handleEntityConfiguration)

	fmt.Printf("Serving on %s\n", conf.ServerAddr)
	if err := http.ListenAndServe(conf.ServerAddr, nil); err != nil {
		log.Fatal(err)
	}
}
