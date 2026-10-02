package secrets

import (
	"encoding/base64"
	"os"
	"testing"

	"github.com/keeper-security/secrets-manager-go/core"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

const validKeeperConfig = `{"hostname":"keepersecurity.com","clientId":"cid","appKey":"akey","privateKey":"pkey"}`

const postgresNotes = `{"type":"postgres","details":{"username":"u","password":"p","host":"h","port":5432,"database":"d"}}`

type mockKeeperNotesGetter struct {
	notes map[string]string
	err   error
	calls int
}

func (m *mockKeeperNotesGetter) GetNotes(title string) (string, error) {
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	n, ok := m.notes[title]
	if !ok {
		return "", errors.New("not found")
	}
	return n, nil
}

type mockKeeperFetcher struct {
	records []*core.Record
	err     error
	calls   int
}

func (m *mockKeeperFetcher) GetSecrets(uids []string) ([]*core.Record, error) {
	m.calls++
	return m.records, m.err
}

func keeperRecord(title, notes string) *core.Record {
	return &core.Record{RecordDict: map[string]any{"title": title, "notes": notes}}
}

func TestNormalizeKeeperConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "json", input: validKeeperConfig},
		{name: "base64", input: base64.StdEncoding.EncodeToString([]byte(validKeeperConfig))},
		{name: "surrounding whitespace", input: "  " + validKeeperConfig + "\n"},
		{name: "empty", input: "  ", wantErr: "empty Keeper configuration"},
		{name: "not base64 or json", input: "%%%", wantErr: "JSON or a base64-encoded JSON"},
		{name: "invalid json", input: "{nope", wantErr: "not valid JSON"},
		{name: "missing key", input: `{"clientId":"c","appKey":"a"}`, wantErr: "missing the 'privateKey' key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeKeeperConfig(tt.input)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.JSONEq(t, validKeeperConfig, got)
		})
	}
}

func TestNewKeeperClientFromEnv(t *testing.T) { //nolint:paralleltest // uses t.Setenv
	log := &mockLogger{}

	t.Run("requires a config source", func(t *testing.T) { //nolint:paralleltest
		t.Setenv(keeperConfigEnvVar, "")
		t.Setenv(keeperConfigFileEnvVar, "")
		client, err := NewKeeperClientFromEnv(log)
		require.ErrorContains(t, err, "must be set")
		require.Nil(t, client)
	})

	t.Run("rejects both config sources", func(t *testing.T) { //nolint:paralleltest
		t.Setenv(keeperConfigEnvVar, validKeeperConfig)
		t.Setenv(keeperConfigFileEnvVar, "/tmp/x.json")
		client, err := NewKeeperClientFromEnv(log)
		require.ErrorContains(t, err, "only one of")
		require.Nil(t, client)
	})

	t.Run("reads inline config", func(t *testing.T) { //nolint:paralleltest
		t.Setenv(keeperConfigEnvVar, validKeeperConfig)
		t.Setenv(keeperConfigFileEnvVar, "")
		client, err := NewKeeperClientFromEnv(log)
		require.NoError(t, err)
		require.NotNil(t, client)
	})

	t.Run("reads config file", func(t *testing.T) { //nolint:paralleltest
		path := t.TempDir() + "/keeper.json"
		require.NoError(t, os.WriteFile(path, []byte(validKeeperConfig), 0o600))
		t.Setenv(keeperConfigEnvVar, "")
		t.Setenv(keeperConfigFileEnvVar, path)
		client, err := NewKeeperClientFromEnv(log)
		require.NoError(t, err)
		require.NotNil(t, client)
	})

	t.Run("missing config file", func(t *testing.T) { //nolint:paralleltest
		t.Setenv(keeperConfigEnvVar, "")
		t.Setenv(keeperConfigFileEnvVar, t.TempDir()+"/missing.json")
		client, err := NewKeeperClientFromEnv(log)
		require.ErrorContains(t, err, "failed to read Keeper config file")
		require.Nil(t, client)
	})
}

func TestKeeperRecordStore(t *testing.T) {
	t.Parallel()

	t.Run("fetches once and serves by title", func(t *testing.T) {
		t.Parallel()
		f := &mockKeeperFetcher{records: []*core.Record{keeperRecord("a", "notes-a"), keeperRecord("b", "notes-b")}}
		s := &keeperRecordStore{fetcher: f}

		got, err := s.GetNotes("a")
		require.NoError(t, err)
		require.Equal(t, "notes-a", got)
		got, err = s.GetNotes("b")
		require.NoError(t, err)
		require.Equal(t, "notes-b", got)
		require.Equal(t, 1, f.calls)
	})

	t.Run("record not found", func(t *testing.T) {
		t.Parallel()
		s := &keeperRecordStore{fetcher: &mockKeeperFetcher{}}
		_, err := s.GetNotes("missing")
		require.ErrorContains(t, err, "no record titled 'missing'")
	})

	t.Run("duplicate titles are rejected", func(t *testing.T) {
		t.Parallel()
		s := &keeperRecordStore{fetcher: &mockKeeperFetcher{records: []*core.Record{keeperRecord("a", "1"), keeperRecord("a", "2")}}}
		_, err := s.GetNotes("a")
		require.ErrorContains(t, err, "must be unique")
	})

	t.Run("fetch failure is retried on next lookup", func(t *testing.T) {
		t.Parallel()
		f := &mockKeeperFetcher{err: errors.New("boom")}
		s := &keeperRecordStore{fetcher: f}
		_, err := s.GetNotes("a")
		require.ErrorContains(t, err, "failed to fetch records from Keeper")

		f.err = nil
		f.records = []*core.Record{keeperRecord("a", "ok")}
		got, err := s.GetNotes("a")
		require.NoError(t, err)
		require.Equal(t, "ok", got)
		require.Equal(t, 2, f.calls)
	})
}

func TestKeeperClient_ResolveConnection(t *testing.T) {
	t.Parallel()

	t.Run("returns a connection and caches it", func(t *testing.T) {
		t.Parallel()
		g := &mockKeeperNotesGetter{notes: map[string]string{"my-pg": postgresNotes}}
		c := newKeeperClientWithGetter(&mockLogger{}, g)

		conn, err := c.ResolveConnection("my-pg")
		require.NoError(t, err)
		require.NotNil(t, conn)

		conn2, err := c.ResolveConnection("my-pg")
		require.NoError(t, err)
		require.Equal(t, conn, conn2)
		require.Equal(t, 1, g.calls)
	})

	t.Run("GetConnection returns nil on failure", func(t *testing.T) {
		t.Parallel()
		c := newKeeperClientWithGetter(&mockLogger{}, &mockKeeperNotesGetter{err: errors.New("denied")})
		require.Nil(t, c.GetConnection("x"))
		require.Nil(t, c.GetConnectionDetails("x"))
		require.Empty(t, c.GetConnectionType("x"))
	})

	t.Run("connection details and type", func(t *testing.T) {
		t.Parallel()
		c := newKeeperClientWithGetter(&mockLogger{}, &mockKeeperNotesGetter{notes: map[string]string{"my-pg": postgresNotes}})
		require.NotNil(t, c.GetConnectionDetails("my-pg"))
		require.Equal(t, "postgres", c.GetConnectionType("my-pg"))
	})

	invalid := map[string]struct{ notes, want string }{
		"empty notes":       {"", "empty notes field"},
		"not json":          {"hello", "not valid JSON"},
		"missing details":   {`{"type":"postgres"}`, "must contain both"},
		"missing type":      {`{"details":{}}`, "must contain both"},
		"details not a map": {`{"type":"postgres","details":"x"}`, "must contain both"},
		"empty type":        {`{"type":"","details":{}}`, "must contain both"},
	}
	for name, tc := range invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newKeeperClientWithGetter(&mockLogger{}, &mockKeeperNotesGetter{notes: map[string]string{"r": tc.notes}})
			_, err := c.ResolveConnection("r")
			require.ErrorContains(t, err, tc.want)
		})
	}
}
