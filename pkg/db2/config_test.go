package db2

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfig_GetIngestrURI(t *testing.T) {
	t.Parallel()

	c := Config{
		Username: "user",
		Password: "password",
		Host:     "localhost",
		Port:     "50000",
		Database: "test",
	}
	assert.Equal(t, "db2://user:password@localhost:50000/test", c.GetIngestrURI())

	c = Config{
		Username: "user",
		Password: "password",
		Host:     "localhost",
		Port:     "446",
		Database: "SIGLPAR",
		Schema:   "GSTDTA",
		SSL:      true,
		Timeout:  15,
		Platform: "ibmi",
	}
	assert.Equal(t, "db2://user:password@localhost:446/SIGLPAR?platform=ibmi&schema=GSTDTA&ssl=true&timeout=15", c.GetIngestrURI())
}
