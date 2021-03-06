// Copyright 2019 Bull S.A.S. Atos Technologies - Bull, Rue Jean Jaures, B.P.68, 78340, Les Clayes-sous-Bois, France.
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

package hostspool

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestExportHostsPool(t *testing.T) {
	err := exportHostsPool(&httpClientMockList{}, nil, "locationOne", "", "")
	require.NoError(t, err, "Failed to export hosts pool")
}

func TestExportHostsPoolWithoutLocation(t *testing.T) {
	err := exportHostsPool(&httpClientMockList{}, nil, "", "", "")
	require.Error(t, err, "Expected error as no location has been provided")
}

func TestExportHostsPoolWithHTTPFailure(t *testing.T) {
	err := exportHostsPool(&httpClientMockList{}, nil, "fails", "", "")
	require.Error(t, err, "Expected error due to HTTP failure")
}

func TestExportHostsPoolJSONError(t *testing.T) {
	err := exportHostsPool(&httpClientMockList{}, nil, "bad_json", "", "")
	require.Error(t, err, "Expected error due to JSON error")
}
