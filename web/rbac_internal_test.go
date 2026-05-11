package web

import (
	"testing"

	"github.com/gorilla/websocket"
)

func TestAuthorizeMessageRoleMatrix(t *testing.T) {
	conn := &websocket.Conn{}
	srv := &Server{
		connStates: map[*websocket.Conn]*connState{
			conn: {role: RoleOperator},
		},
	}

	if err := srv.authorizeMessage(conn, "primitiveOp"); err != nil {
		t.Fatalf("expected operator primitiveOp to be allowed, got %v", err)
	}
	if err := srv.authorizeMessage(conn, "requestFullRefresh"); err != nil {
		t.Fatalf("expected operator refresh to be allowed, got %v", err)
	}
	if err := srv.authorizeMessage(conn, "createMutex"); err == nil {
		t.Fatal("expected operator create to be denied")
	}
	if err := srv.authorizeMessage(conn, "deletePrimitive"); err == nil {
		t.Fatal("expected operator delete to be denied")
	}
}

func TestAuthorizeMessageDefaultsToAdminWithoutAuthState(t *testing.T) {
	conn := &websocket.Conn{}
	srv := &Server{
		connStates: map[*websocket.Conn]*connState{
			conn: {},
		},
	}

	if err := srv.authorizeMessage(conn, "createMutex"); err != nil {
		t.Fatalf("expected default admin create to be allowed, got %v", err)
	}
	if err := srv.authorizeMessage(conn, "primitiveOp"); err != nil {
		t.Fatalf("expected default admin primitiveOp to be allowed, got %v", err)
	}
}

func TestNormalizeRoleJWTFallbacks(t *testing.T) {
	if got := normalizeRole("", true); got != RoleViewer {
		t.Fatalf("expected empty JWT role to default to viewer, got %q", got)
	}
	if got := normalizeRole("superuser", true); got != RoleViewer {
		t.Fatalf("expected unknown JWT role to default to viewer, got %q", got)
	}
	if got := normalizeRole("", false); got != RoleAdmin {
		t.Fatalf("expected unauthenticated role to default to admin, got %q", got)
	}
}
