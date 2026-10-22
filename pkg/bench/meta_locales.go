package bench

import (
	"embed"
	"fmt"

	"gpuworks.ai/gpuworks/pkg/localization"
)

var requiredBenchmarkLocales = []string{localization.English, localization.Chinese}

//go:embed locales/*.json
var benchmarkLocaleFiles embed.FS

type benchmarkLocaleEntry struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type benchmarkLocale struct {
	Modes           map[string]benchmarkLocaleEntry `json:"modes"`
	Datasets        map[string]benchmarkLocaleEntry `json:"datasets"`
	RetiredDatasets map[string]benchmarkLocaleEntry `json:"retired_datasets"`
	SLAConstraints  map[string]string               `json:"sla_constraints"`
	Objectives      map[string]benchmarkLocaleEntry `json:"objectives"`
	Statuses        map[string]benchmarkLocaleEntry `json:"statuses"`
}

var benchmarkLocales map[string]benchmarkLocale

func init() {
	locales, err := localization.LoadJSONFiles[benchmarkLocale](benchmarkLocaleFiles, "locales/%s.json", requiredBenchmarkLocales)
	if err == nil {
		err = validateBenchmarkLocales(dictionaryDefinition(), locales)
	}
	if err != nil {
		panic(fmt.Sprintf("load benchmark locales: %v", err))
	}
	benchmarkLocales = locales
}

func validateBenchmarkLocales(meta Meta, locales map[string]benchmarkLocale) error {
	for _, language := range requiredBenchmarkLocales {
		locale, ok := locales[language]
		if !ok {
			return fmt.Errorf("missing required locale %q", language)
		}
		for _, section := range []struct {
			name               string
			options            []Option
			entries            map[string]benchmarkLocaleEntry
			requireDescription bool
		}{
			{name: "mode", options: meta.Modes, entries: locale.Modes, requireDescription: true},
			{name: "dataset", options: meta.Datasets, entries: locale.Datasets, requireDescription: true},
			{name: "retired dataset", options: meta.RetiredDatasets, entries: locale.RetiredDatasets, requireDescription: true},
			{name: "objective", options: meta.Objectives, entries: locale.Objectives},
			{name: "status", options: meta.Statuses, entries: locale.Statuses},
		} {
			if err := validateBenchmarkOptionSection(language, section.name, section.options, section.entries, section.requireDescription); err != nil {
				return err
			}
		}
		constraintKeys := make(map[string]struct{}, len(meta.SLAConstraints))
		for _, field := range meta.SLAConstraints {
			constraintKeys[field.Key] = struct{}{}
		}
		if err := validateBenchmarkLabels(language, "SLA constraint", constraintKeys, locale.SLAConstraints); err != nil {
			return err
		}
	}
	return nil
}

func validateBenchmarkOptionSection(language, kind string, options []Option, entries map[string]benchmarkLocaleEntry, requireDescription bool) error {
	expected := make(map[string]struct{}, len(options))
	for _, option := range options {
		expected[option.Value] = struct{}{}
	}
	labels := make(map[string]string, len(entries))
	for key, entry := range entries {
		labels[key] = entry.Label
		if requireDescription && entry.Description == "" {
			return fmt.Errorf("locale %s %s %q has no description", language, kind, key)
		}
		if !requireDescription && entry.Description != "" {
			return fmt.Errorf("locale %s %s %q has an unexpected description", language, kind, key)
		}
	}
	return validateBenchmarkLabels(language, kind, expected, labels)
}

func validateBenchmarkLabels(language, kind string, expected map[string]struct{}, labels map[string]string) error {
	for key := range labels {
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("locale %s contains unknown %s %q", language, kind, key)
		}
	}
	for key := range expected {
		if labels[key] == "" {
			return fmt.Errorf("locale %s is missing %s %q", language, kind, key)
		}
	}
	return nil
}

func localizeMeta(meta Meta) Meta {
	meta.Modes = localizeBenchmarkOptions(meta.Modes, func(locale benchmarkLocale) map[string]benchmarkLocaleEntry { return locale.Modes })
	meta.Datasets = localizeBenchmarkOptions(meta.Datasets, func(locale benchmarkLocale) map[string]benchmarkLocaleEntry { return locale.Datasets })
	meta.RetiredDatasets = localizeBenchmarkOptions(meta.RetiredDatasets, func(locale benchmarkLocale) map[string]benchmarkLocaleEntry { return locale.RetiredDatasets })
	meta.Objectives = localizeBenchmarkOptions(meta.Objectives, func(locale benchmarkLocale) map[string]benchmarkLocaleEntry { return locale.Objectives })
	meta.Statuses = localizeBenchmarkOptions(meta.Statuses, func(locale benchmarkLocale) map[string]benchmarkLocaleEntry { return locale.Statuses })
	for index := range meta.SLAConstraints {
		field := &meta.SLAConstraints[index]
		field.Label = make(localization.Text, len(requiredBenchmarkLocales))
		for _, language := range requiredBenchmarkLocales {
			field.Label[language] = benchmarkLocales[language].SLAConstraints[field.Key]
		}
	}
	return meta
}

func localizeBenchmarkOptions(options []Option, section func(benchmarkLocale) map[string]benchmarkLocaleEntry) []Option {
	for index := range options {
		option := &options[index]
		option.Label = make(localization.Text, len(requiredBenchmarkLocales))
		for _, language := range requiredBenchmarkLocales {
			entry := section(benchmarkLocales[language])[option.Value]
			option.Label[language] = entry.Label
			if entry.Description != "" {
				if option.Description == nil {
					option.Description = make(localization.Text, len(requiredBenchmarkLocales))
				}
				option.Description[language] = entry.Description
			}
		}
	}
	return options
}
