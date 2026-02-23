package constants

const (
	TokenRoleAdmin    = "admin"
	TokenRoleReadOnly = "read-only"
)

var AllowedTokenRoles = []string{
	TokenRoleAdmin,
	TokenRoleReadOnly,
}
