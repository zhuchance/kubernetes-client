/**
 * Copyright (C) 2015 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *         http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package integration

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"testing"

	knet "k8s.io/apimachinery/pkg/util/net"

	configapi "github.com/openshift/origin/pkg/cmd/server/apis/config"
	testserver "github.com/openshift/origin/test/util/server"
)

// simulate embedding the given string in a template href
func templateEscapeHref(test *testing.T, s string) string {
	prefix := `<a href="`
	suffix := `">`

	b := new(bytes.Buffer)
	t := template.Must(template.New("foo").Parse(fmt.Sprintf(`%s{{.}}%s`, prefix, suffix)))
	if err := t.Execute(b, s); err != nil {
		test.Fatalf("unexpected error escaping %s: %v", s, err)
		return ""
	}

	escaped := b.String()
	return escaped[len(prefix) : len(escaped)-len(suffix)]
}

func tryAccessURL(t *testing.T, url string, expectedStatus int, expectedRedirectLocation string, expectedLinks []string) *http.Response {
	transport := knet.SetTransportDefaults(&http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	})

	req, err := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "text/html")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Errorf("Unexpected error while accessing %q: %v", url, err)
		return nil
	}
	if resp.StatusCode != expectedStatus {
		t.Errorf("Expected status %d for %q, got %d", expectedStatus, url, resp.StatusCode)
	}
	// ignore query parameters
	location := resp.Header.Get("Location")
	location = strings.SplitN(location, "?", 2)[0]
	if location != expectedRedirectLocation {
		t.Errorf("Expected redirection to %q for %q, got %q instead", expectedRedirectLocation, url, location)
	}

	if expectedLinks != nil {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("failed to read reposponse's body: %v", err)
		} else {
			for _, linkRegexp := range expectedLinks {
				matched, err := regexp.Match(linkRegexp, body)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				} else if !matched {
					t.Errorf("Expected response body to match %s", linkRegexp)
					t.Logf("Response body was %s", body)
				}
			}
		}
	}

	return resp
}

func TestAccessOriginWebConsoleMultipleIdentityProviders(t *testing.T) {
	masterOptions, err := testserver.DefaultMasterOptions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Replace the default IdentityProvider with an AllowAll provider
	masterOptions.OAuthConfig.IdentityProviders[0] = configapi.IdentityProvider{
		Name:            "foo",
		UseAsChallenger: true,
		UseAsLogin:      true,
		MappingMethod:   "claim",
		Provider:        &configapi.AllowAllPasswordIdentityProvider{},
	}

	// Set up a second AllowAll provider
	masterOptions.OAuthConfig.IdentityProviders = append(masterOptions.OAuthConfig.IdentityProviders, configapi.IdentityProvider{
		Name:            "bar",
		UseAsChallenger: true,
		UseAsLogin:      true,
		MappingMethod:   "claim",
		Provider:        &configapi.AllowAllPasswordIdentityProvider{},
	})

	// Set up a third AllowAll provider with a space in the name and some unicode characters
	masterOptions.OAuthConfig.IdentityProviders = append(masterOptions.OAuthConfig.IdentityProviders, configapi.IdentityProvider{
		Name:            "Iñtërnâtiônàlizætiøn, !@#$^&*()",
		UseAsChallenger: true,
		UseAsLogin:      true,
		MappingMethod:   "claim",
		Provider:        &configapi.AllowAllPasswordIdentityProvider{},
	})

	// Launch the configured server
	if _, err := testserver.StartConfiguredMaster(masterOptions); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterOptions)

	// Create a map of URLs to test
	type urlResults struct {
		statusCode int
		location   string
	}

	urlMap := make(map[string]urlResults)
	linkRegexps := make([]string, 0)

	// Verify that the plain /login URI is unavailable when multiple IDPs are in use.
	urlMap["/login"] = urlResults{http.StatusNotFound, ""}

	// Create the common base URLs
	escapedPublicURL := url.QueryEscape(masterOptions.OAuthConfig.AssetPublicURL)
	loginSelectorBase := "/oauth/authorize?client_id=openshift-web-console&response_type=token&state=%2F&redirect_uri=" + escapedPublicURL

	// Iterate through each of the providers and verify that they redirect to
	// the appropriate login page and that the login page exists.
	// This is done in a loop so that we can add an arbitrary additional set
	// of providers to test.
	for _, value := range masterOptions.OAuthConfig.IdentityProviders {
		// Query-encode the idp=<provider name> parameter name and value
		idpQueryParam := url.Values{"idp": []string{value.Name}}.Encode()

		// Construct a URL that will select that IDP
		providerSelectionURL := loginSelectorBase + "&" + idpQueryParam

		// URL-path-encode the idp name to construct the login page URL
		loginURL := (&url.URL{Path: path.Join("/login", value.Name)}).String()

		// Expect the providerSelectionURL to redirect to the loginURL
		urlMap[providerSelectionURL] = urlResults{http.StatusFound, loginURL}
		// Expect the loginURL to be valid (requires a 'then' param)
		urlMap[loginURL+"?then=%2F"] = urlResults{http.StatusOK, ""}

		// escape the query param the way the template will
		templateIDPParam := templateEscapeHref(t, idpQueryParam)
		// quote for the regex
		regexIDPParam := regexp.QuoteMeta(templateIDPParam)
		// Expect to see a link to the provider selection page URL with the idp param
		linkRegexps = append(linkRegexps, fmt.Sprintf(`/oauth/authorize\?(.*&amp;)?%s(&amp;|")`, regexIDPParam))
	}

	// Test the loginSelectorBase for links to all of the IDPs
	url := masterOptions.OAuthConfig.MasterPublicURL + loginSelectorBase
	tryAccessURL(t, url, http.StatusOK, "", linkRegexps)

	// Test all of these URLs
	for endpoint, exp := range urlMap {
		url := masterOptions.OAuthConfig.MasterPublicURL + endpoint
		tryAccessURL(t, url, exp.statusCode, exp.location, nil)
	}
}