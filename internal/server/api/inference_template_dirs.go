package api

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// managedDirsView is the spec's declared directories as the template DTO
// shows them; nil when it declares none or the column does not decode.
func managedDirsView(raw string) []dto.InferenceManagedDir {
	dirs, err := deploy.DecodeManagedDirs(raw)
	if err != nil || len(dirs) == 0 {
		return nil
	}
	out := make([]dto.InferenceManagedDir, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, dto.InferenceManagedDir{Name: dir.Name, MountPath: dir.MountPath, LimitBytes: dir.LimitBytes, Retain: dir.Retain})
	}
	return out
}
