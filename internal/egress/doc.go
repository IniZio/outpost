// Package egress gives each box generation its credentials for the proxy, and answers the proxy's
// grant requests. A box holds only placeholders and a proxy login bound to its task and box
// generation. The proxy trades that login for a grant: the task's repos and the owner's secrets
// for each credential host rule. Nothing per task is stored: the login is an HMAC under a subkey
// of the server's master key.
package egress
