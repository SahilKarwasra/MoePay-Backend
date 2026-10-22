package httpx

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Keys under which the auth middleware stores the authenticated principal.
const (
	ContextUserIDKey    = "auth.user_id"
	ContextSessionIDKey = "auth.session_id"
)

// Principal is the authenticated caller.
type Principal struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
}

func SetPrincipal(c *gin.Context, p Principal) {
	c.Set(ContextUserIDKey, p.UserID.String())
	c.Set(ContextSessionIDKey, p.SessionID.String())
	c.Set("auth.principal", p)
}

// MustPrincipal returns the principal set by the auth middleware. It panics if the
// route was not wrapped by it, which is a programming error caught by Recover.
func MustPrincipal(c *gin.Context) Principal {
	p, ok := c.Get("auth.principal")
	if !ok {
		panic("httpx.MustPrincipal called on unauthenticated route")
	}
	return p.(Principal)
}
