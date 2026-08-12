// Copyright 2026 The SandboxFleet Authors.
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

package kata

import (
	"context"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

// Files ride the kata-agent exec channel, the only way into a self-managed guest.

func (r *Runtime) ReadFile(ctx context.Context, id sandboxruntime.ID, absPath string) ([]byte, error) {
	return sandboxruntime.ReadFileVia(ctx, r.execFunc(id), absPath)
}

func (r *Runtime) WriteFile(ctx context.Context, id sandboxruntime.ID, absPath string, content []byte) error {
	return sandboxruntime.WriteFileVia(ctx, r.execFunc(id), absPath, content)
}

func (r *Runtime) ListFiles(ctx context.Context, id sandboxruntime.ID, absPath string) ([]sandboxruntime.FileEntry, error) {
	return sandboxruntime.ListFilesVia(ctx, r.execFunc(id), absPath)
}

func (r *Runtime) FileExists(ctx context.Context, id sandboxruntime.ID, absPath string) (bool, error) {
	return sandboxruntime.FileExistsVia(ctx, r.execFunc(id), absPath)
}

func (r *Runtime) execFunc(id sandboxruntime.ID) sandboxruntime.ExecFunc {
	return func(ctx context.Context, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
		return r.Exec(ctx, id, req)
	}
}
