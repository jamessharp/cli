package identity_test

import (
	"testing"

	"github.com/ory/x/cmdx"

	"github.com/ory/cli/cmd/cloudx/testhelpers"
)

var (
	defaultProject, defaultConfig, defaultEmail, defaultPassword string
	defaultCmd                                                   *cmdx.CommandExecuter
)

func TestMain(m *testing.M) {
	defaultConfig, defaultEmail, defaultPassword, defaultProject, defaultCmd = testhelpers.CreateDefaultAssets()
	testhelpers.RunAgainstStaging(m)
}