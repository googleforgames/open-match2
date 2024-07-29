// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logging

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestNewFormatter(t *testing.T) {
	testCases := []struct {
		in       string
		expected interface{}
	}{
		{"", &logrus.TextFormatter{}},
		{"json", &logrus.JSONFormatter{}},
		{"text", &logrus.TextFormatter{}},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(fmt.Sprintf("newFormatter(%s) => %s", tc.in, reflect.TypeOf(tc.expected)), func(t *testing.T) {
			require := require.New(t)
			actual := newFormatter(tc.in)
			require.Equal(reflect.TypeOf(tc.expected), reflect.TypeOf(actual))
		})
	}
}
