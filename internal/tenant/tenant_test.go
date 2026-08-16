package tenant

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTenantStatusString(t *testing.T) {
	cases := []struct {
		status TenantStatus
		want   string
	}{
		{TenantActive, "active"},
		{TenantSuspended, "suspended"},
		{TenantDeleted, "deleted"},
		{TenantStatus(99), "unknown(99)"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.status.String())
	}
}

func TestParseTenantStatus(t *testing.T) {
	cases := []struct {
		in   string
		want TenantStatus
		err  bool
	}{
		{"active", TenantActive, false},
		{"SUSPENDED", TenantSuspended, false},
		{"  deleted  ", TenantDeleted, false},
		{"bogus", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := ParseTenantStatus(c.in)
		if c.err {
			assert.Error(t, err)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, c.want, got)
	}
}

func TestNewTenant(t *testing.T) {
	tt := NewTenant("acme", "ACME Corp")
	assert.NotEmpty(t, tt.ID)
	assert.True(t, strings.HasPrefix(tt.ID, "tenant-"))
	assert.Equal(t, "acme", tt.Name)
	assert.Equal(t, "ACME Corp", tt.DisplayName)
	assert.Equal(t, "tenant-acme", tt.Namespace)
	assert.Equal(t, TenantActive, tt.Status)
	assert.False(t, tt.CreatedAt.IsZero())
	assert.Equal(t, tt.CreatedAt, tt.UpdatedAt)
	assert.NotNil(t, tt.Labels)
}

func TestNamespaceFor(t *testing.T) {
	assert.Equal(t, "tenant-acme", NamespaceFor("acme"))
	assert.Equal(t, "tenant-", NamespaceFor(""))
}

func TestTenantValidate(t *testing.T) {
	cases := []struct {
		name    string
		tenant  *Tenant
		wantErr bool
		errSub  string
	}{
		{
			name:   "valid",
			tenant: NewTenant("acme", "ACME"),
		},
		{
			name:    "nil",
			tenant:  nil,
			wantErr: true,
			errSub:  "nil tenant",
		},
		{
			name: "empty id",
			tenant: func() *Tenant {
				tt := NewTenant("acme", "ACME")
				tt.ID = ""
				return tt
			}(),
			wantErr: true,
			errSub:  "empty id",
		},
		{
			name: "empty name",
			tenant: func() *Tenant {
				tt := NewTenant("acme", "ACME")
				tt.Name = ""
				return tt
			}(),
			wantErr: true,
			errSub:  "empty name",
		},
		{
			name: "invalid name uppercase",
			tenant: func() *Tenant {
				tt := NewTenant("ACME", "ACME")
				return tt
			}(),
			wantErr: true,
			errSub:  "must match",
		},
		{
			name: "invalid name single char",
			tenant: func() *Tenant {
				tt := NewTenant("a", "A")
				return tt
			}(),
			wantErr: true,
			errSub:  "must match",
		},
		{
			name: "invalid name underscore",
			tenant: func() *Tenant {
				tt := NewTenant("ac_me", "ACME")
				return tt
			}(),
			wantErr: true,
			errSub:  "must match",
		},
		{
			name: "invalid namespace mismatch",
			tenant: func() *Tenant {
				tt := NewTenant("acme", "ACME")
				tt.Namespace = "tenant-other"
				return tt
			}(),
			wantErr: true,
			errSub:  "does not match",
		},
		{
			name:   "valid with hyphen",
			tenant: NewTenant("acme-prod", "ACME Prod"),
		},
		{
			name:   "valid with digits",
			tenant: NewTenant("acme-123", "ACME 123"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.tenant.Validate()
			if c.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.errSub)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestTenantIsOperational(t *testing.T) {
	tt := NewTenant("acme", "ACME")
	assert.True(t, tt.IsOperational())

	tt.Status = TenantSuspended
	assert.False(t, tt.IsOperational())

	tt.Status = TenantDeleted
	assert.False(t, tt.IsOperational())

	var nilTenant *Tenant
	assert.False(t, nilTenant.IsOperational())
}

func TestContextWithTenant(t *testing.T) {
	ctx := context.Background()
	ctx = ContextWithTenant(ctx, "tenant-abc")

	got, err := TenantFromContext(ctx)
	require.NoError(t, err)
	assert.Equal(t, "tenant-abc", got)
}

func TestContextWithTenantNilContext(t *testing.T) {
	ctx := ContextWithTenant(nil, "tenant-abc")
	got, err := TenantFromContext(ctx)
	require.NoError(t, err)
	assert.Equal(t, "tenant-abc", got)
}

func TestTenantFromContextMissing(t *testing.T) {
	_, err := TenantFromContext(context.Background())
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestTenantFromContextNil(t *testing.T) {
	_, err := TenantFromContext(nil)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestTenantFromContextEmpty(t *testing.T) {
	ctx := ContextWithTenant(context.Background(), "")
	_, err := TenantFromContext(ctx)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestMustTenantFromContext(t *testing.T) {
	ctx := ContextWithTenant(context.Background(), "tenant-x")
	assert.NotPanics(t, func() {
		id := MustTenantFromContext(ctx)
		assert.Equal(t, "tenant-x", id)
	})
}

func TestMustTenantFromContextPanics(t *testing.T) {
	assert.Panics(t, func() {
		_ = MustTenantFromContext(context.Background())
	})
}

func TestNewTenantIDUniqueness(t *testing.T) {
	ids := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := newTenantID()
		assert.True(t, strings.HasPrefix(id, "tenant-"))
		_, dup := ids[id]
		assert.False(t, dup, "duplicate id generated: %s", id)
		ids[id] = struct{}{}
	}
}

func TestEncodeDecodeTenantTag(t *testing.T) {
	cases := []struct {
		tenantID string
		incident string
	}{
		{"t1", ""},
		{"t1", "inc-123"},
		{"t1", "inc-with|pipe"},
	}
	for _, c := range cases {
		encoded := EncodeTenantTag(c.tenantID, c.incident)
		assert.True(t, strings.HasPrefix(encoded, "tenant:"+c.tenantID))
		tid, inc := DecodeTenantTag(encoded)
		assert.Equal(t, c.tenantID, tid)
		assert.Equal(t, c.incident, inc)
	}
}

func TestDecodeTenantTagLegacy(t *testing.T) {
	// A legacy incident id without the tenant: prefix should round-trip
	// as ("", original).
	tid, inc := DecodeTenantTag("inc-legacy")
	assert.Empty(t, tid)
	assert.Equal(t, "inc-legacy", inc)
}
