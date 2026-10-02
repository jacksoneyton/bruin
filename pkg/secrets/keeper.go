package secrets

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"sync"

	"github.com/bruin-data/bruin/pkg/config"
	"github.com/bruin-data/bruin/pkg/connection"
	"github.com/bruin-data/bruin/pkg/logger"
	"github.com/keeper-security/secrets-manager-go/core"
	"github.com/pkg/errors"
)

const (
	keeperConfigEnvVar     = "BRUIN_KEEPER_CONFIG"
	keeperConfigFileEnvVar = "BRUIN_KEEPER_CONFIG_FILE"
)

// keeperRequiredConfigKeys are the keys a Keeper Secrets Manager client configuration must contain.
var keeperRequiredConfigKeys = []string{"clientId", "appKey", "privateKey"}

// keeperNotesGetter returns the notes of the Keeper record with the given title.
type keeperNotesGetter interface {
	GetNotes(title string) (string, error)
}

// keeperSecretsFetcher is the subset of the Keeper Secrets Manager SDK used by Bruin.
type keeperSecretsFetcher interface {
	GetSecrets(uids []string) ([]*core.Record, error)
}

// keeperRecordStore loads every record shared with the Keeper application once and serves
// lookups by record title from memory, so a run makes a single API call regardless of
// how many connections it resolves.
type keeperRecordStore struct {
	fetcher keeperSecretsFetcher

	mu           sync.Mutex
	loaded       bool
	notesByTitle map[string][]string
}

func (s *keeperRecordStore) GetNotes(title string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.loaded {
		records, err := s.fetcher.GetSecrets(nil)
		if err != nil {
			return "", errors.Wrap(err, "failed to fetch records from Keeper")
		}

		index := make(map[string][]string, len(records))
		for _, record := range records {
			index[record.Title()] = append(index[record.Title()], record.Notes())
		}
		s.notesByTitle = index
		s.loaded = true
	}

	notes, ok := s.notesByTitle[title]
	if !ok {
		return "", errors.Errorf("no record titled '%s' found; make sure it is shared with the Keeper application", title)
	}
	if len(notes) > 1 {
		return "", errors.Errorf("%d records are titled '%s'; record titles must be unique", len(notes), title)
	}

	return notes[0], nil
}

// KeeperClient manages secrets from Keeper Secrets Manager.
type KeeperClient struct {
	client                  keeperNotesGetter
	logger                  logger.Logger
	cacheMu                 sync.RWMutex
	cacheConnections        map[string]any
	cacheConnectionsDetails map[string]any
}

// NewKeeperClientFromEnv creates a new Keeper client from environment variables.
// The client configuration is read from BRUIN_KEEPER_CONFIG (base64 or JSON) or, alternatively,
// from the file referenced by BRUIN_KEEPER_CONFIG_FILE.
func NewKeeperClientFromEnv(logger logger.Logger) (*KeeperClient, error) {
	inline := strings.TrimSpace(os.Getenv(keeperConfigEnvVar))
	path := strings.TrimSpace(os.Getenv(keeperConfigFileEnvVar))

	switch {
	case inline != "" && path != "":
		return nil, errors.Errorf("only one of %s and %s can be set", keeperConfigEnvVar, keeperConfigFileEnvVar)
	case inline == "" && path == "":
		return nil, errors.Errorf("%s or %s env variable must be set", keeperConfigEnvVar, keeperConfigFileEnvVar)
	case path != "":
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read Keeper config file '%s'", path)
		}
		inline = string(content)
	}

	return NewKeeperClient(logger, inline)
}

// NewKeeperClient creates a Keeper client from a Keeper Secrets Manager client configuration,
// given either as JSON or as a base64-encoded JSON string.
func NewKeeperClient(logger logger.Logger, configValue string) (*KeeperClient, error) {
	configJSON, err := normalizeKeeperConfig(configValue)
	if err != nil {
		return nil, err
	}

	// The configuration is always passed explicitly so the SDK never falls back to KSM_CONFIG or its default config file.
	sm := core.NewSecretsManager(&core.ClientOptions{
		Config: core.NewMemoryKeyValueStorage(configJSON),
	})
	if sm == nil {
		return nil, errors.New("failed to initialize Keeper Secrets Manager client from the provided configuration")
	}

	return newKeeperClientWithGetter(logger, &keeperRecordStore{fetcher: sm}), nil
}

func newKeeperClientWithGetter(logger logger.Logger, getter keeperNotesGetter) *KeeperClient {
	return &KeeperClient{
		client:                  getter,
		logger:                  logger,
		cacheConnections:        make(map[string]any),
		cacheConnectionsDetails: make(map[string]any),
	}
}

// normalizeKeeperConfig accepts the configuration as JSON or base64-encoded JSON and returns validated JSON.
func normalizeKeeperConfig(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty Keeper configuration provided")
	}

	if !strings.HasPrefix(value, "{") {
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return "", errors.New("Keeper configuration must be JSON or a base64-encoded JSON string")
		}
		value = strings.TrimSpace(string(decoded))
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return "", errors.Wrap(err, "Keeper configuration is not valid JSON")
	}

	for _, key := range keeperRequiredConfigKeys {
		if s, ok := parsed[key].(string); !ok || s == "" {
			return "", errors.Errorf("Keeper configuration is missing the '%s' key", key)
		}
	}

	return value, nil
}

// GetConnection retrieves a connection by name from Keeper.
func (c *KeeperClient) GetConnection(name string) any {
	conn, err := c.ResolveConnection(name)
	if err != nil {
		c.logger.Errorf("%v", err)
		return nil
	}

	return conn
}

// ResolveConnection implements config.ConnectionResolver.
func (c *KeeperClient) ResolveConnection(name string) (any, error) {
	c.cacheMu.RLock()
	if conn, ok := c.cacheConnections[name]; ok {
		c.cacheMu.RUnlock()
		return conn, nil
	}
	c.cacheMu.RUnlock()

	manager, err := c.getKeeperManager(name)
	if err != nil {
		return nil, err
	}

	conn := manager.GetConnection(name)

	c.cacheMu.Lock()
	c.cacheConnections[name] = conn
	c.cacheMu.Unlock()

	return conn, nil
}

// GetConnectionDetails retrieves connection details by name from Keeper.
func (c *KeeperClient) GetConnectionDetails(name string) any {
	c.cacheMu.RLock()
	if deets, ok := c.cacheConnectionsDetails[name]; ok {
		c.cacheMu.RUnlock()
		return deets
	}
	c.cacheMu.RUnlock()

	manager, err := c.getKeeperManager(name)
	if err != nil {
		c.logger.Errorf("%v", err)
		return nil
	}

	deets := manager.GetConnectionDetails(name)

	c.cacheMu.Lock()
	c.cacheConnectionsDetails[name] = deets
	c.cacheMu.Unlock()

	return deets
}

func (c *KeeperClient) GetConnectionType(name string) string {
	manager, err := c.getKeeperManager(name)
	if err != nil {
		return ""
	}
	return manager.GetConnectionType(name)
}

func (c *KeeperClient) getKeeperManager(name string) (config.ConnectionAndDetailsGetter, error) {
	notes, err := c.client.GetNotes(name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read secret '%s' from Keeper", name)
	}

	if strings.TrimSpace(notes) == "" {
		return nil, errors.Errorf("record '%s' has an empty notes field; expected a JSON document with 'type' and 'details'", name)
	}

	var secretData map[string]any
	if err := json.Unmarshal([]byte(notes), &secretData); err != nil {
		return nil, errors.Errorf("notes of record '%s' are not valid JSON", name)
	}

	detailsRaw, okDetails := secretData["details"]
	secretType, okType := secretData["type"].(string)

	details, detailsIsMap := detailsRaw.(map[string]any)
	if !okDetails || !detailsIsMap || !okType || secretType == "" {
		return nil, errors.Errorf("record '%s' must contain both 'type' (non-empty string) and 'details' (object) in its notes", name)
	}

	details["name"] = name

	connectionsMap := map[string][]map[string]any{
		secretType: {
			details,
		},
	}

	serialized, err := json.Marshal(connectionsMap)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to process secret '%s'", name)
	}

	var connections config.Connections

	if err := json.Unmarshal(serialized, &connections); err != nil {
		return nil, errors.Wrapf(err, "failed to parse secret '%s' configuration", name)
	}

	environment := config.Environment{
		Connections: &connections,
	}

	cfg := config.Config{
		Environments: map[string]config.Environment{
			"default": environment,
		},
		SelectedEnvironmentName: "default",
		SelectedEnvironment:     &environment,
		DefaultEnvironmentName:  "default",
	}

	manager, errs := connection.NewManagerFromConfig(&cfg)
	if len(errs) > 0 {
		return nil, errors.Wrapf(errs[0], "failed to configure connection '%s'", name)
	}

	return manager, nil
}
