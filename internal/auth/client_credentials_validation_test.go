package auth

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

func TestValidateClientCredentialsAccessToken(t *testing.T) {
	c := qt.New(t)

	newToken := func(claims map[string]any) jwt.Token {
		builder := jwt.NewBuilder()
		for key, value := range claims {
			builder = builder.Claim(key, value)
		}
		tok, err := builder.Build()
		c.Assert(err, qt.IsNil)
		return tok
	}

	testCases := []struct {
		name                 string
		token                jwt.Token
		audience             string
		expectedErrSubstring string
	}{
		{
			name: "valid azp claim",
			token: newToken(map[string]any{
				"iss": "https://issuer.example",
				"aud": []string{"jimm-api", "other-audience"},
				"azp": "client-a",
			}),
			audience: "jimm-api",
		},
		{
			name: "valid client_id fallback",
			token: newToken(map[string]any{
				"iss":       "https://issuer.example",
				"aud":       "jimm-api",
				"client_id": "client-a",
			}),
			audience: "jimm-api",
		},
		{
			name: "invalid issuer",
			token: newToken(map[string]any{
				"iss": "https://other-issuer.example",
				"aud": "jimm-api",
				"azp": "client-a",
			}),
			audience:             "jimm-api",
			expectedErrSubstring: "invalid issuer claim",
		},
		{
			name: "missing audience claim",
			token: newToken(map[string]any{
				"iss": "https://issuer.example",
				"azp": "client-a",
			}),
			audience:             "jimm-api",
			expectedErrSubstring: "missing audience claim",
		},
		{
			name: "audience mismatch",
			token: newToken(map[string]any{
				"iss": "https://issuer.example",
				"aud": []string{"other-audience"},
				"azp": "client-a",
			}),
			audience:             "jimm-api",
			expectedErrSubstring: "does not contain expected audience",
		},
		{
			name: "azp mismatch",
			token: newToken(map[string]any{
				"iss": "https://issuer.example",
				"aud": "jimm-api",
				"azp": "other-client",
			}),
			audience:             "jimm-api",
			expectedErrSubstring: "azp claim does not match client id",
		},
		{
			name: "missing azp and client_id",
			token: newToken(map[string]any{
				"iss": "https://issuer.example",
				"aud": "jimm-api",
			}),
			audience:             "jimm-api",
			expectedErrSubstring: "missing authorized party claim",
		},
		{
			name: "audience check disabled when unset",
			token: newToken(map[string]any{
				"iss": "https://issuer.example",
				"azp": "client-a",
			}),
			audience: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			authSvc := &AuthenticationService{
				issuerURL:                "https://issuer.example",
				clientCredentialAudience: tc.audience,
			}

			err := authSvc.validateClientCredentialsAccessToken(tc.token, "client-a")
			if tc.expectedErrSubstring == "" {
				c.Assert(err, qt.IsNil)
				return
			}

			c.Assert(err, qt.IsNotNil)
			c.Assert(err.Error(), qt.Contains, tc.expectedErrSubstring)
		})
	}
}
