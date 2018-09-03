// Copyright 2018 Bull S.A.S. Atos Technologies - Bull, Rue Jean Jaures, B.P.68, 78340, Les Clayes-sous-Bois, France.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build !premium

package ansible

import (
	"context"
	"fmt"
	"io"
)

// Open source version of a handler implementing the interface outputHandler.
// This handler logs the output of a script execution, once this script has been
// executed.
// The handler is started before the script is run by the orchestrator,
// and stopped once the script was run.
type scriptOutputHandler struct {
	execution *executionScript
	context   context.Context
}

func (h *scriptOutputHandler) getWrappedCommand() string {
	wrappedCmd := fmt.Sprintf("{{ ansible_env.HOME}}/%s/wrapper",
		h.execution.OperationRemotePath)
	return wrappedCmd
}

func (h *scriptOutputHandler) start(cmdReader io.ReadCloser) error {
	go logAnsibleOutputInConsulFromScript(h.context, h.execution.deploymentID,
		h.execution.NodeName, h.execution.hosts, cmdReader)
	return nil
}

func (h *scriptOutputHandler) stop() error {
	return nil
}
