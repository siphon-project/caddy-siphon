package postgresstorage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestCaddyfileParsesEveryOption(t *testing.T) {
	d := caddyfile.NewTestDispenser(`postgresql {
		dsn "{env.CADDY_STORAGE_DSN}"
		table_prefix acme_
		lock_ttl 45s
		auto_create false
	}`)
	var s Storage
	if err := s.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if s.DSN != "{env.CADDY_STORAGE_DSN}" || s.TablePrefix != "acme_" ||
		time.Duration(s.LockTTL) != 45*time.Second || s.AutoCreate == nil || *s.AutoCreate {
		t.Fatalf("parsed dsn=%q prefix=%q ttl=%v auto_create=%v", s.DSN, s.TablePrefix, s.LockTTL, s.AutoCreate)
	}
	encoded, err := json.Marshal(&s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"dsn":"{env.CADDY_STORAGE_DSN}","table_prefix":"acme_","lock_ttl":45000000000,"auto_create":false}`
	if string(encoded) != want {
		t.Errorf("json = %s, want %s", encoded, want)
	}
}

func TestCaddyfileRefusesMistakes(t *testing.T) {
	for _, input := range []string{
		"postgresql",                                  // no dsn
		"postgresql some-arg {\n dsn x\n}",            // stray argument
		"postgresql {\n dsn\n}",                       // missing value
		"postgresql {\n dsn a b\n}",                   // extra value
		"postgresql {\n dsn x\n colour blue\n}",       // unknown option
		"postgresql {\n dsn x\n auto_create maybe\n}", // not a boolean
		"postgresql {\n dsn x\n lock_ttl soon\n}",     // not a duration
	} {
		var s Storage
		if err := s.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestValidateRefusesBadPrefixAndShortTTL(t *testing.T) {
	for _, s := range []*Storage{
		{},
		{DSN: "x", TablePrefix: "Caddy"},
		{DSN: "x", TablePrefix: `a"; DROP TABLE x; --`},
		{DSN: "x", TablePrefix: "1abc"},
		{DSN: "x", TablePrefix: strings.Repeat("a", 57)},
		{DSN: "x", LockTTL: caddy.Duration(500 * time.Millisecond)},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("accepted prefix=%q ttl=%v dsn=%q", s.TablePrefix, s.LockTTL, s.DSN)
		}
	}
	ok := Storage{DSN: "x", TablePrefix: "caddy_", LockTTL: caddy.Duration(time.Second)}
	if err := ok.Validate(); err != nil {
		t.Errorf("refused a good config: %v", err)
	}
}

func TestStringNeverShowsTheDSN(t *testing.T) {
	s := Storage{DSN: "postgres://caddy:secret@db.example.com/pbx"}
	if strings.Contains(s.String(), "secret") {
		t.Errorf("String() leaks the dsn: %s", s.String())
	}
}
