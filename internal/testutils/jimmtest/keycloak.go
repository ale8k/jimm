// Copyright 2025 Canonical.

package jimmtest

// These constants are based on the `docker-compose.yaml` and `local/keycloak/jimm-realm.json` content.
const (
	// HardcodedSafeUsername is a hardcoded test keycloak user that pre-exists
	// but is safe for use in a Juju UserTag when the email is retrieved.
	HardcodedSafeUsername = "jimm-test"
	HardcodedSafePassword = "password"
	// HardcodedUnsafeUsername is a hardcoded test keycloak user that pre-exists
	// but is unsafe for use in a Juju UserTag when the email is retrieved.
	HardcodedUnsafeUsername = "jimm_test"
	HardcodedUnsafePassword = "password"
	// HardcodedGroupUsername is a hardcoded test keycloak user that pre-exists
	// and belongs to OIDCGroupsTestGroupName.
	HardcodedGroupUsername = "jimm-group-user"
	HardcodedGroupEmail    = "jimm-group-user@canonical.com"
	HardcodedGroupPassword = "password"
	HardcodedGroupUserID   = "jimm-group-user-id"

	// OIDCGroupsTestGroupName is the group used by real Keycloak-backed tests
	// that verify OIDC group claim extraction.
	OIDCGroupsTestGroupName = "canonical"

	// HardcodedServiceAccountClientID and HardcodedServiceAccountClientSecret
	// are valid Keycloak service-account credentials for local tests.
	HardcodedServiceAccountClientID     = "test-client-id"
	HardcodedServiceAccountClientSecret = "2M2blFbO4GX4zfggQpivQSxwWX1XGgNf"

	// HardcodedServiceAccountWithGroupsClientID and
	// HardcodedServiceAccountWithGroupsClientSecret are valid Keycloak
	// service-account credentials for a client with seeded groups.
	HardcodedServiceAccountWithGroupsClientID     = "test-client-id-with-groups"
	HardcodedServiceAccountWithGroupsClientSecret = "d6JVj6NDXYx56muG6ZmjWMLcJnIYQjD0"
)
