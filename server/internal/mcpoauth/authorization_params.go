// Added by the mouse9527/multica fork: extra authorization request parameters for MCP entries (M-295 stage 3).

package mcpoauth

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// AuthorizationParams are the extra parameters an entry may add to the
// authorization request:
//
//	"auth": {"type": "oauth", ..., "authorization_params": {"prompt": "consent", "access_type": "offline"}}
//
// Only these keys are allowed. They are widely used by authorization servers
// (OpenID Connect Core §3.1.2.1 for prompt, display, login_hint and
// ui_locales; Google for access_type and hd) and none of them is a parameter
// the protocol itself sets, so an entry can never replace response_type,
// client_id, redirect_uri, state, the PKCE challenge, scope, resource or
// audience through them. A struct rather than a map keeps Entry comparable.
type AuthorizationParams struct {
	Prompt     string
	AccessType string
	LoginHint  string
	Display    string
	UILocales  string
	HD         string
}

// authorizationParamKeys is the allowlist, in the order the parameters are
// listed in errors and added to the request.
var authorizationParamKeys = []string{"prompt", "access_type", "login_hint", "display", "ui_locales", "hd"}

// protocolParamKeys are the authorization request parameters the connect
// sets itself; an entry that names one gets an error saying so.
var protocolParamKeys = map[string]bool{
	"response_type": true, "client_id": true, "redirect_uri": true, "state": true,
	"code_challenge": true, "code_challenge_method": true, "scope": true, "resource": true, "audience": true,
}

func (p *AuthorizationParams) field(key string) *string {
	switch key {
	case "prompt":
		return &p.Prompt
	case "access_type":
		return &p.AccessType
	case "login_hint":
		return &p.LoginHint
	case "display":
		return &p.Display
	case "ui_locales":
		return &p.UILocales
	case "hd":
		return &p.HD
	}
	return nil
}

// values are the set parameters as key/value pairs, in allowlist order.
func (p AuthorizationParams) values() [][2]string {
	var out [][2]string
	for _, key := range authorizationParamKeys {
		if value := *p.field(key); value != "" {
			out = append(out, [2]string{key, value})
		}
	}
	return out
}

// parseAuthorizationParams reads auth.authorization_params. Errors name the
// rule, never a value; an unknown key is not echoed either, since it is
// whatever the entry's author typed.
func parseAuthorizationParams(raw json.RawMessage) (AuthorizationParams, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return AuthorizationParams{}, fmt.Errorf("%w: auth.authorization_params must be an object", ErrInvalidInput)
	}
	var out AuthorizationParams
	for key, value := range fields {
		if protocolParamKeys[key] {
			return AuthorizationParams{}, fmt.Errorf("%w: auth.authorization_params cannot set %s: the connect sets it", ErrInvalidInput, key)
		}
		target := out.field(key)
		if target == nil {
			return AuthorizationParams{}, fmt.Errorf("%w: auth.authorization_params may only contain %s", ErrInvalidInput,
				strings.Join(authorizationParamKeys, ", "))
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			return AuthorizationParams{}, fmt.Errorf("%w: auth.authorization_params.%s must be a string", ErrInvalidInput, key)
		}
		text = strings.TrimSpace(text)
		if err := validateParamValue("authorization_params."+key, text, maxScopeBytes); err != nil {
			return AuthorizationParams{}, err
		}
		*target = text
	}
	return out, nil
}

// withAuthorizationParams adds the audience and the extra parameters to an
// authorization URL. They never collide with the protocol's own parameters
// (parseAuthorizationParams refuses those keys).
func withAuthorizationParams(authorizationURL string, server ServerConfig) (string, error) {
	params := server.Params.values()
	if server.Audience == "" && len(params) == 0 {
		return authorizationURL, nil
	}
	u, err := url.Parse(authorizationURL)
	if err != nil {
		return "", err
	}
	query := u.Query()
	if server.Audience != "" {
		query.Set("audience", server.Audience)
	}
	for _, param := range params {
		query.Set(param[0], param[1])
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}
