package auth

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

func TestPasswordChangeRevokesAllTokens(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	s := New(st.Users(), st.Roles())
	if err := s.Bootstrap(ctx, "before-password"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Login(ctx, "admin", "before-password")
	b, _ := s.Login(ctx, "admin", "before-password")
	if err := s.ChangePassword(ctx, a.UserID, "before-password", "after-password"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{a.Token, b.Token} {
		if _, _, err := s.UserByToken(ctx, token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("old token accepted: %v", err)
		}
	}
	if _, err := s.Login(ctx, "admin", "after-password"); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledUserRejectsExistingTokenEvenAcrossServices(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	s := New(st.Users(), st.Roles())
	if err := s.Bootstrap(ctx, "admin-password"); err != nil {
		t.Fatal(err)
	}
	roles, _ := s.ListRoles(ctx)
	var roleID string
	for _, r := range roles {
		if r.Name == RoleUser {
			roleID = r.ID
		}
	}
	u, err := s.CreateUser(ctx, CreateUserRequest{Username: "worker", Password: "worker-password", RoleID: roleID})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := s.Login(ctx, "worker", "worker-password")
	other := New(st.Users(), st.Roles())
	status := "disabled"
	if _, err := other.UpdateUser(ctx, u.ID, UpdateUserRequest{Status: &status}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UserByToken(ctx, session.Token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled user's token accepted: %v", err)
	}
	status = "active"
	if _, err := other.UpdateUser(ctx, u.ID, UpdateUserRequest{Status: &status}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UserByToken(ctx, session.Token); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("reactivation resurrected old token")
	}
}

func TestCustomNamedLastLocalAdminProtected(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	s := New(st.Users(), st.Roles())
	if err := s.BootstrapWithName(ctx, "emergency-owner", "admin-password"); err != nil {
		t.Fatal(err)
	}
	u, _ := st.Users().GetByUsername(ctx, "emergency-owner")
	status := "disabled"
	if _, err := s.UpdateUser(ctx, u.ID, UpdateUserRequest{Status: &status}); !errors.Is(err, ErrProtectedUser) {
		t.Fatalf("last admin disabled: %v", err)
	}
	if err := s.DeleteUser(ctx, u.ID); !errors.Is(err, ErrProtectedUser) {
		t.Fatalf("last admin deleted: %v", err)
	}
}

func TestStaleUserUpdateCannotRestorePassword(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	s := New(st.Users(), st.Roles())
	if err := s.Bootstrap(ctx, "before-password"); err != nil {
		t.Fatal(err)
	}
	stale, _ := st.Users().GetByUsername(ctx, "admin")
	if err := s.ChangePassword(ctx, stale.ID, "before-password", "after-password"); err != nil {
		t.Fatal(err)
	}
	stale.DisplayName = "stale form"
	if err := st.Users().Update(ctx, stale); !errors.Is(err, store.ErrConcurrentUpdate) {
		t.Fatalf("stale credentials restored: %v", err)
	}
}

func TestConcurrentAdminDisableLeavesLocalRecovery(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	s := New(st.Users(), st.Roles())
	if err := s.BootstrapWithName(ctx, "first-owner", "first-password"); err != nil {
		t.Fatal(err)
	}
	first, _ := st.Users().GetByUsername(ctx, "first-owner")
	second, err := s.CreateUser(ctx, CreateUserRequest{Username: "second-owner", Password: "second-password", RoleID: first.RoleID})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{first.ID, second.ID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			status := "disabled"
			_, err := s.UpdateUser(ctx, id, UpdateUserRequest{Status: &status})
			results <- err
		}(id)
	}
	wg.Wait()
	close(results)
	succeeded, protected := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrProtectedUser) {
			protected++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || protected != 1 {
		t.Fatalf("succeeded=%d protected=%d", succeeded, protected)
	}
}

func TestBootstrapRejectsExistingDatabaseWithoutLocalAdmin(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	s := New(st.Users(), st.Roles())
	if err := s.Bootstrap(ctx, "admin-password"); err != nil {
		t.Fatal(err)
	}
	admin, _ := st.Users().GetByUsername(ctx, "admin")
	roles, _ := s.ListRoles(ctx)
	var userRole string
	for _, r := range roles {
		if r.Name == RoleUser {
			userRole = r.ID
		}
	}
	if _, err := s.CreateUser(ctx, CreateUserRequest{Username: "ordinary", Password: "ordinary-password", RoleID: userRole}); err != nil {
		t.Fatal(err)
	}
	// Simulate externally damaged data, bypassing the application service.
	if err := st.Users().Delete(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(ctx, ""); err == nil {
		t.Fatal("database without recovery administrator started normally")
	}
}
