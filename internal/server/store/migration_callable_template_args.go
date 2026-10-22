package store

import (
	"encoding/json"
	"strings"

	"gorm.io/gorm"
)

// callableServeArgs are the arguments the callable engine used to put in front
// of every adapter's template arguments, written as the template variables
// that now state the same facts (docs/adr/0036-callable-command-owned-by-template.md).
var callableServeArgs = []string{"--model", "${MODEL_PATH}", "--host", "${API_HOST}", "--port", "${API_PORT}"}

// migrateCallableTemplateArgs is migration 130. The callable engine stopped
// appending --model/--host/--port; a template now places them with template
// variables, and a launch whose template never references ${API_PORT} is
// refused. Every callable deployment created before this change carries a
// catalog selection snapshot taken from such a template, so its snapshot's
// template arguments get the old arguments in front, in the old order: the
// next launch produces the argv the deployment always had instead of being
// refused.
//
// A snapshot that already references ${API_PORT} anywhere in its launch
// fields was written by a new template and is left alone, which also makes the
// migration idempotent. Only extra_args changes; every other key of the
// document, including ones this build does not know, is written back as it
// was read. A document that does not decode is left exactly as it is: the
// launch path reports it the same way it did before.
func migrateCallableTemplateArgs(db *gorm.DB) error {
	type row struct {
		ID       uint
		Snapshot string `gorm:"column:template_snapshot"`
	}
	if !db.Migrator().HasTable("deployments") {
		return nil
	}
	var rows []row
	if err := db.Table("deployments").Select("id, template_snapshot").
		Where("engine = ? AND template_snapshot <> ''", "callable").Find(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		document := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(r.Snapshot), &document); err != nil || document == nil {
			continue
		}
		var launch struct {
			ExtraArgs      []string          `json:"extra_args"`
			Env            map[string]string `json:"env"`
			PreStartArgv   []string          `json:"pre_start_argv"`
			PreStartScript string            `json:"pre_start_script"`
			Entrypoint     []string          `json:"entrypoint"`
		}
		if err := json.Unmarshal([]byte(r.Snapshot), &launch); err != nil {
			continue
		}
		fields := append(append(append([]string{launch.PreStartScript}, launch.ExtraArgs...), launch.PreStartArgv...), launch.Entrypoint...)
		for _, value := range launch.Env {
			fields = append(fields, value)
		}
		if strings.Contains(strings.Join(fields, "\x00"), "${API_PORT}") {
			continue
		}
		args, err := json.Marshal(append(append([]string(nil), callableServeArgs...), launch.ExtraArgs...))
		if err != nil {
			return err
		}
		document["extra_args"] = args
		encoded, err := json.Marshal(document)
		if err != nil {
			return err
		}
		if err := db.Table("deployments").Where("id = ?", r.ID).Update("template_snapshot", string(encoded)).Error; err != nil {
			return err
		}
	}
	return nil
}
