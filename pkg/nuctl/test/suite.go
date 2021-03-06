/*
Copyright 2017 The Nuclio Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/nuclio/nuclio/pkg/cmdrunner"
	"github.com/nuclio/nuclio/pkg/common"
	"github.com/nuclio/nuclio/pkg/dockerclient"
	"github.com/nuclio/nuclio/pkg/functionconfig"
	"github.com/nuclio/nuclio/pkg/nuctl/command"
	"github.com/nuclio/nuclio/pkg/version"

	"github.com/ghodss/yaml"
	"github.com/nuclio/logger"
	"github.com/nuclio/zap"
	"github.com/stretchr/testify/suite"
)

const (
	nuctlPlatformEnvVarName = "NUCTL_PLATFORM"
)

type Suite struct {
	suite.Suite
	origPlatformType    string
	logger              logger.Logger
	rootCommandeer      *command.RootCommandeer
	dockerClient        dockerclient.Client
	shellClient         *cmdrunner.ShellRunner
	outputBuffer        bytes.Buffer
	inputBuffer         bytes.Buffer
	defaultWaitDuration time.Duration
	defaultWaitInterval time.Duration
}

func (suite *Suite) SetupSuite() {
	var err error

	// update version so that linker doesn't need to inject it
	version.SetFromEnv()

	// create logger
	suite.logger, err = nucliozap.NewNuclioZapTest("test")
	suite.Require().NoError(err)

	// create shell runner
	suite.shellClient, err = cmdrunner.NewShellRunner(suite.logger)
	suite.Require().NoError(err)

	// create docker client
	suite.dockerClient, err = dockerclient.NewShellClient(suite.logger, suite.shellClient)
	suite.Require().NoError(err)

	// save platform type before the test
	suite.origPlatformType = os.Getenv(nuctlPlatformEnvVarName)

	// init default wait values to be used during tests for retries
	suite.defaultWaitDuration = 1 * time.Minute
	suite.defaultWaitInterval = 5 * time.Second

	// default to local platform if platform isn't set
	if os.Getenv(nuctlPlatformEnvVarName) == "" {
		err = os.Setenv(nuctlPlatformEnvVarName, "local")
		suite.Require().NoError(err)
	}
}

func (suite *Suite) SetupTest() {
	suite.outputBuffer.Reset()
	suite.inputBuffer.Reset()
}

func (suite *Suite) TearDownSuite() {

	// restore platform type
	err := os.Setenv(nuctlPlatformEnvVarName, suite.origPlatformType)
	suite.Require().NoError(err)
}

// ExecuteNuctl populates os.Args and executes nuctl as if it were executed from shell
func (suite *Suite) ExecuteNuctl(positionalArgs []string,
	namedArgs map[string]string) error {

	suite.rootCommandeer = command.NewRootCommandeer()

	// set the output so we can capture it (but also output to stdout)
	suite.rootCommandeer.GetCmd().SetOut(io.MultiWriter(os.Stdout, &suite.outputBuffer))

	// set the input so we can write to stdin
	suite.rootCommandeer.GetCmd().SetIn(&suite.inputBuffer)

	// since args[0] is the executable name, just shove something there
	argsStringSlice := []string{
		"nuctl",
	}

	// add positional arguments
	argsStringSlice = append(argsStringSlice, positionalArgs...)

	for argName, argValue := range namedArgs {
		argsStringSlice = append(argsStringSlice, fmt.Sprintf("--%s", argName), argValue)
	}

	// override os.Args (this can't go wrong horribly, can it?)
	os.Args = argsStringSlice

	suite.logger.DebugWith("Executing nuctl", "args", argsStringSlice)

	// execute
	return suite.rootCommandeer.Execute()
}

// ExecuteNuctl populates os.Args and executes nuctl as if it were executed from shell
func (suite *Suite) ExecuteNuctlAndWait(positionalArgs []string,
	namedArgs map[string]string,
	expectFailure bool) error {

	return common.RetryUntilSuccessful(suite.defaultWaitDuration,
		suite.defaultWaitInterval,
		func() bool {

			// execute
			err := suite.ExecuteNuctl(positionalArgs, namedArgs)
			if expectFailure {
				return err != nil
			}
			return err == nil
		})
}

// GetNuclioSourceDir returns path to nuclio source directory
func (suite *Suite) GetNuclioSourceDir() string {
	return common.GetSourceDir()
}

// GetNuclioSourceDir returns path to nuclio source directory
func (suite *Suite) GetFunctionsDir() string {
	return path.Join(suite.GetNuclioSourceDir(), "test", "_functions")
}

func (suite *Suite) GetFunctionConfigsDir() string {
	return path.Join(suite.GetNuclioSourceDir(), "test", "_function_configs")
}

func (suite *Suite) GetImportsDir() string {
	return path.Join(suite.GetNuclioSourceDir(), "test", "_imports")
}

func (suite *Suite) findPatternsInOutput(patternsMustExist []string, patternsMustNotExist []string) {
	foundPatternsMustExist := make([]bool, len(patternsMustExist))
	foundPatternsMustNotExist := make([]bool, len(patternsMustNotExist))

	// iterate over all lines in result
	scanner := bufio.NewScanner(&suite.outputBuffer)
	for scanner.Scan() {

		for patternIdx, patternName := range patternsMustExist {
			if strings.Contains(scanner.Text(), patternName) {
				foundPatternsMustExist[patternIdx] = true
				break
			}
		}

		for patternIdx, patternName := range patternsMustNotExist {
			if strings.Contains(scanner.Text(), patternName) {
				foundPatternsMustNotExist[patternIdx] = true
				break
			}
		}
	}

	// all patterns that must exist must exist
	for _, foundPattern := range foundPatternsMustExist {
		suite.Require().True(foundPattern)
	}

	// all patterns that must not exist must not exist
	for _, foundPattern := range foundPatternsMustNotExist {
		suite.Require().False(foundPattern)
	}
}

func (suite *Suite) assertFunctionImported(functionName string, imported bool) {

	// reset output buffer for reading the nex output cleanly
	suite.outputBuffer.Reset()
	err := suite.ExecuteNuctlAndWait([]string{"get", "function", functionName}, map[string]string{
		"output": "yaml",
	}, false)
	suite.Require().NoError(err)

	function := functionconfig.Config{}
	functionBodyBytes := suite.outputBuffer.Bytes()
	err = yaml.Unmarshal(functionBodyBytes, &function)
	suite.Require().NoError(err)

	suite.Assert().Equal(functionName, function.Meta.Name)
	if imported {

		// get imported functions
		err = suite.ExecuteNuctl([]string{"get", "function", functionName}, nil)
		suite.Require().NoError(err)

		// ensure function state is imported
		suite.findPatternsInOutput([]string{"imported"}, nil)
	}
}
