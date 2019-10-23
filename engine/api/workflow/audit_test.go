package workflow

import (
	"testing"

	"github.com/ovh/cds/engine/api/test"
)

func Test_purgeAudits(t *testing.T) {
	db, _, end := test.SetupPG(t)
	defer end()

	err := purgeAudits(db)
	test.NoError(t, err)
}
