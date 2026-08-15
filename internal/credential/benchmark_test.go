package credential

import (
	"context"
	"crypto/rand"

	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/state"
)

// --- Encryption benchmarks ---

func BenchmarkCredentialStore_encrypt(b *testing.B) {
	b.ReportAllocs()
	dbPath := filepath.Join(b.TempDir(), "bench_cred.db")
	store, err := state.NewSQLiteStore(context.Background(), dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	cs, err := NewCredentialStore(store, "bench-master-password")
	if err != nil {
		b.Fatal(err)
	}

	plaintext := []byte("my-super-secret-password-1234567890")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cs.encrypt(plaintext)
	}
}

func BenchmarkCredentialStore_decrypt(b *testing.B) {
	b.ReportAllocs()
	dbPath := filepath.Join(b.TempDir(), "bench_cred_decrypt.db")
	store, err := state.NewSQLiteStore(context.Background(), dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	cs, err := NewCredentialStore(store, "bench-master-password")
	if err != nil {
		b.Fatal(err)
	}

	plaintext := []byte("my-super-secret-password-1234567890")
	blob, _ := cs.encrypt(plaintext)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cs.decrypt(blob)
	}
}

func BenchmarkCredentialStore_deriveKey(b *testing.B) {
	b.ReportAllocs()
	dbPath := filepath.Join(b.TempDir(), "bench_cred_key.db")
	store, err := state.NewSQLiteStore(context.Background(), dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	cs, err := NewCredentialStore(store, "bench-master-password")
	if err != nil {
		b.Fatal(err)
	}

	salt := make([]byte, saltLen)
	_, _ = rand.Read(salt)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := cs.deriveKey(salt)
		SecureZero(key)
	}
}

func BenchmarkSecureZero(b *testing.B) {
	b.ReportAllocs()
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		SecureZero(data)
		for j := range data {
			data[j] = byte(j)
		}
	}
}

func BenchmarkCredentialStore_StoreAndRetrieve(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "bench_cred_full.db")
	store, err := state.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	cs, err := NewCredentialStore(store, "bench-master-password")
	if err != nil {
		b.Fatal(err)
	}

	// Pre-store a credential.
	pt := []byte("my-secret-password")
	_, _ = cs.Store(ctx, CredentialSpec{
		Name:      "bench-cred",
		Type:      "ssh_password",
		Plaintext: pt,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cs.Retrieve(ctx, "bench-cred")
	}
}

func BenchmarkNewID(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = newID()
	}
}
