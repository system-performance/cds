package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	jwt "github.com/dgrijalva/jwt-go"
	"github.com/gorilla/mux"

	"github.com/ovh/cds/engine/api/authentication"
	"github.com/ovh/cds/engine/api/observability"
	"github.com/ovh/cds/engine/service"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/log"
)

const (
	jwtCookieName  = "jwt_token"
	xsrfHeaderName = "X-XSRF-TOKEN"
	xsrfCookieName = "xsrf_token"
)

func (api *API) authMiddleware(ctx context.Context, w http.ResponseWriter, req *http.Request, rc *service.HandlerConfig) (context.Context, error) {
	ctx, end := observability.Span(ctx, "router.authMiddleware")
	defer end()

	// Tokens (like izanamy)
	ctx, ok, err := api.authStatusTokenMiddleware(ctx, w, req, rc)
	if err != nil {
		return ctx, sdk.WithStack(err)
	}
	if ok {
		log.Info("authentification granted by token")
		return ctx, nil
	}

	// Check for a JWT in current request and add it to the context
	// If a JWT is given, we also checks that there are a valid session and consumer for it
	ctx, err = api.jwtMiddleware(ctx, w, req, rc)
	if err != nil {
		return ctx, err
	}
	jwt, ok := ctx.Value(contextJWT).(*jwt.Token)
	if ok {
		claims := jwt.Claims.(*sdk.AuthSessionJWTClaims)
		sessionID := claims.StandardClaims.Id

		// Check for session based on jwt from context
		session, err := authentication.CheckSession(ctx, api.mustDB(), sessionID)
		if err != nil {
			return ctx, sdk.NewErrorWithStack(err, sdk.ErrUnauthorized)
		}
		ctx = context.WithValue(ctx, contextSession, session)

		// Load auth consumer for current session in database
		consumer, err := authentication.LoadConsumerByID(ctx, api.mustDB(), session.ConsumerID,
			authentication.LoadConsumerOptions.WithAuthentifiedUser)
		if err != nil {
			return ctx, sdk.NewErrorWithStack(err, sdk.ErrUnauthorized)
		}
		ctx = context.WithValue(ctx, contextAPIConsumer, consumer)

		// Checks scopes, all expected scopes should be in actual scopes
		// Actual scope empty list means wildcard scope, we don't need to check scopes
		expectedScopes, actualScopes := rc.AllowedScopes, consumer.Scopes
		if len(expectedScopes) > 0 && len(actualScopes) > 0 {
			var found bool
		findScope:
			for i := range expectedScopes {
				for j := range actualScopes {
					if actualScopes[j] == expectedScopes[i] {
						found = true
						break findScope
					}
				}
			}
			if !found {
				return ctx, sdk.WrapError(sdk.ErrUnauthorized, "token scope (%v) doesn't match (%v)", actualScopes, expectedScopes)
			}
		}

		// Check that permission are valid for current route and consumer
		if err := api.checkPermission(ctx, mux.Vars(req), rc.PermissionLevel); err != nil {
			return ctx, err
		}
	}

	// If the route don't need auth return directly
	if rc.NeedAuth && getAPIConsumer(ctx) == nil {
		return nil, sdk.WithStack(sdk.ErrUnauthorized)
	}

	if rc.NeedAdmin && !isAdmin(ctx) {
		return ctx, sdk.WithStack(sdk.ErrForbidden)
	}

	return ctx, nil
}

// Checks static tokens
func (api *API) authStatusTokenMiddleware(ctx context.Context, w http.ResponseWriter, req *http.Request, rc *service.HandlerConfig) (context.Context, bool, error) {
	if len(rc.AllowedTokens) == 0 {
		return ctx, false, nil
	}
	for _, h := range rc.AllowedTokens {
		log.Debug("checking allowed token: %v", h)
		headerSplitted := strings.Split(h, ":")
		receivedValue := req.Header.Get(headerSplitted[0])
		if receivedValue != headerSplitted[1] {
			return ctx, false, sdk.WrapError(sdk.ErrUnauthorized, "Router> Authorization denied token on %s %s for %s", req.Method, req.URL, req.RemoteAddr)
		}
	}
	return ctx, true, nil
}

func (api *API) jwtMiddleware(ctx context.Context, w http.ResponseWriter, req *http.Request, rc *service.HandlerConfig) (context.Context, error) {
	ctx, end := observability.Span(ctx, "router.jwtMiddleware")
	defer end()

	var jwtRaw string
	var xsrfTokenNeeded bool

	// Try to get the jwt from the cookie firstly then from the authorization bearer header, a XSRF token with cookie
	jwtCookie, _ := req.Cookie(jwtCookieName)
	if jwtCookie != nil {
		jwtRaw = jwtCookie.Value
		xsrfTokenNeeded = true
	} else if strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		jwtRaw = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	}
	// If no jwt is given, simply return empty context without error
	if jwtRaw == "" {
		return ctx, nil
	}

	jwt, err := authentication.CheckSessionJWT(jwtRaw)
	if err != nil {
		return ctx, err
	}
	claims := jwt.Claims.(*sdk.AuthSessionJWTClaims)
	sessionID := claims.StandardClaims.Id

	// Checking X-XSRF-TOKEN header if needed and permission level higher than read
	if xsrfTokenNeeded {
		log.Debug("authJWTMiddleware> searching for a xsrf token in header")
		xsrfToken := req.Header.Get(xsrfHeaderName)

		log.Debug("authJWTMiddleware> searching for a xsrf token in cache")
		existingXSRFToken, existXSRFTokenInCache := authentication.GetSessionXSRFToken(api.Cache, sessionID)

		// If it's not a read request we want to check the xsrf token then generate a new one
		// else if its a read request we want to reuse a cached XSRF token or generate one
		if rc.PermissionLevel > sdk.PermissionRead {
			log.Debug("authJWTMiddleware> checking xsrf token")

			if !existXSRFTokenInCache || xsrfToken != existingXSRFToken {
				return ctx, sdk.WithStack(sdk.ErrUnauthorized)
			}

			newXSRFToken := authentication.NewSessionXSRFToken(api.Cache, sessionID)
			// Set a cookie with the jwt token
			http.SetCookie(w, &http.Cookie{
				Name:    xsrfCookieName,
				Value:   newXSRFToken,
				Expires: time.Now().Add(time.Duration(authentication.XSRFTokenDuration) * time.Second),
				Path:    "/",
			})
		} else {
			if !existXSRFTokenInCache {
				existingXSRFToken = authentication.NewSessionXSRFToken(api.Cache, sessionID)
			}

			// Set a cookie with the jwt token
			http.SetCookie(w, &http.Cookie{
				Name:    xsrfCookieName,
				Value:   existingXSRFToken,
				Expires: time.Now().Add(time.Duration(authentication.XSRFTokenDuration) * time.Second),
				Path:    "/",
			})
		}
	}

	ctx = context.WithValue(ctx, contextJWTRaw, jwt)
	ctx = context.WithValue(ctx, contextJWT, jwt)

	return ctx, nil
}
