// Added by the mouse9527/multica fork: RFC 9207 authorization response issuer validation (M-295 stage 2).

package mcpoauth

import (
	"encoding/base64"
	"errors"
	"strings"
)

// The MCP authorization spec requires a client to record, before sending
// the browser to the authorization server, the issuer of the validated
// metadata, "associated with the same per-request record used to store the
// PKCE code verifier (and the state value)", and to check the iss parameter
// of the authorization response against it (RFC 9207 §2.4) before the code
// goes to any token endpoint.
//
// The record is the state itself: a connect's state is the random value
// followed by the expected issuer,
//
//	<random>.<"1" if the server advertises iss, else "0"><issuer, base64url>
//
// and only the SHA-256 of the whole state is stored (mcp_oauth_state). A
// state whose issuer part was changed in transit hashes to no pending
// authorization and is refused like any unknown state, so the issuer read
// back is the one recorded. This needs no new column on mcp_oauth_state.
const stateIssuerSeparator = "."

// ErrResponseIssuer: the authorization response's iss is not the issuer the
// connect started with, or is missing where the server said it sends one.
// The response is not acted on — not even its error.
var ErrResponseIssuer = errors.New("the authorization response did not come from the authorization server this connect started with")

// stateWithIssuer appends the expected issuer to a state's random part.
// Without an issuer (nothing to check against) the state is the random part.
func stateWithIssuer(random, issuer string, issRequired bool) string {
	if issuer == "" {
		return random
	}
	flag := "0"
	if issRequired {
		flag = "1"
	}
	return random + stateIssuerSeparator + flag + base64.RawURLEncoding.EncodeToString([]byte(issuer))
}

// issuerFromState reads back what stateWithIssuer recorded. A state without
// an issuer part, or one that does not decode, records none.
func issuerFromState(state string) (issuer string, issRequired bool) {
	_, recorded, found := strings.Cut(state, stateIssuerSeparator)
	if !found || len(recorded) < 2 || (recorded[0] != '0' && recorded[0] != '1') {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(recorded[1:])
	if err != nil {
		return "", false
	}
	return string(decoded), recorded[0] == '1'
}

// checkResponseIssuer applies RFC 9207 §2.4 as the MCP authorization spec
// tabulates it: a present iss must equal the recorded issuer by simple
// string comparison, whatever the metadata advertised; an absent iss is
// refused only when the server advertised that it sends one.
func checkResponseIssuer(p PendingAuthorization, iss string) error {
	if p.Issuer == "" {
		return nil
	}
	if iss != "" {
		if iss != p.Issuer {
			return ErrResponseIssuer
		}
		return nil
	}
	if p.ISSRequired {
		return ErrResponseIssuer
	}
	return nil
}
