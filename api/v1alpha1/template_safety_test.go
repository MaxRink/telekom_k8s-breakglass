// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "testing"

func TestTemplateOutputSafety(t *testing.T) {
	for _, source := range []string{
		`{{ .vars.payload }}`, `{{ index .vars "payload" }}`,
		`{{ $x := .vars.payload }}{{ $x }}`,
		`{{ with .vars }}{{ .payload }}{{ end }}`,
		`{{ range .vars }}{{ . }}{{ end }}`,
		`{{ define "emit" }}{{ . }}{{ end }}{{ template "emit" .vars.payload }}`,
		`{{ .vars.payload | quote | b64dec }}`,
		`{{ printf "%s" .vars.payload }}{{/* yamlQuote */}}`,
		`{{ .annotations }}`, `{{ .session.approvedBy }}`,
		`{{ .vars.payload | fromYaml | toYaml }}`,
		`{{ with .vars.payload | fromYaml }}{{ .session.name }}{{ end }}`,
		`{{ env "HOME" }}`, `{{ expandenv "$HOME" }}`,
	} {
		t.Run(source, func(t *testing.T) {
			if err := validateGoTemplateSyntax(source); err == nil {
				t.Fatal("accepted unsafe output")
			}
		})
	}
	for _, source := range []string{
		`{{ .vars.payload | yamlQuote }}`,
		`{{ .vars.payload | b64dec | yamlQuote }}`,
		`{{ $x := .vars.payload }}{{ $x | yamlQuote }}`,
		`{{ with .vars }}{{ .payload | yamlQuote }}{{ end }}`,
		`{{ range .vars }}
- {{ . | yamlQuote }}
{{ end }}`,
		`{{ define "emit" }}{{ . | yamlQuote }}{{ end }}{{ template "emit" .vars.payload }}`,
		`{{ if eq .vars.choice "a,b" }}yes{{ else }}no{{ end }}`,
		`{{ eq .vars.choice "true" }}`, `{{ .vars.count | int }}`,
		`{{ .session.name }}`, `{{ 42 }}`, `{{ true }}`,
		`{{ repeat 1000000000 "x" | yamlQuote }}`,
	} {
		t.Run(source, func(t *testing.T) {
			if err := validateGoTemplateSyntax(source); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTemplateScalarPlacement(t *testing.T) {
	for _, source := range []string{
		`value: "{{ .vars.payload | yamlQuote }}"`,
		`value: '{{ .vars.payload | yamlQuote }}'`,
		`value: prefix{{ .vars.payload | yamlQuote }}`,
		`value: {{ .vars.payload | yamlQuote }}suffix`,
		`value: {{ .vars.payload | yamlQuote }}{{ .session.name }}`,
		`value: {{ if .vars.flag }}"{{ end }}{{ .vars.payload | yamlQuote }}`,
		`{{ define "emit" }}{{ . | yamlQuote }}{{ end }}value: "{{ template "emit" .vars.payload }}"`,
		`{{ $x := .vars.payload }}value: "{{ $x | yamlQuote }}"`,
		`value: [prefix
{{ .vars.payload | yamlQuote }}]`,
		`value: 'a''b
{{ .vars.payload | yamlQuote }}'`,
		`{{ ("\"") }}
{{ .vars.payload | yamlQuote }}`,
	} {
		t.Run(source, func(t *testing.T) {
			if err := validateGoTemplateSyntax(source); err == nil {
				t.Fatal("accepted incomplete scalar placement")
			}
		})
	}
	for _, source := range []string{
		`value: {{ .vars.payload | yamlQuote }}`,
		`values: [{{ .vars.payload | yamlQuote }}, {{ .vars.other | yamlQuote }}]`,
		`{{ define "emit" }}{{ . | yamlQuote }}{{ end }}value: {{ template "emit" .vars.payload }}`,
		`value: {{ if .vars.flag }}{{ .vars.payload | yamlQuote }}{{ else }}{{ .vars.other | yamlQuote }}{{ end }}`,
		`{{ range .vars }}
- {{ . | yamlQuote }}
{{ end }}`,
		`name: prefix-{{ .session.name }}
count: {{ .vars.count | int }}`,
	} {
		t.Run(source, func(t *testing.T) {
			if err := validateGoTemplateSyntax(source); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTemplateMutationDisablesTrustedFields(t *testing.T) {
	for _, source := range []string{
		`value: {{ set .session "name" .vars.payload }}{{ .session.name }}`,
		`{{ if .vars.flag }}{{ set .session "name" .vars.payload }}{{ end }}value: {{ .session.name }}`,
		`{{ with .vars }}{{ set $.session "name" .payload }}{{ end }}value: {{ .session.name }}`,
		`{{ range .vars.items }}{{ merge $.session . }}{{ end }}value: {{ .session.name }}`,
		`{{ define "mutate" }}{{ mustMerge .session .vars }}{{ end }}value: {{ .session.name }}`,
		`{{ define "mutate" }}{{ .session.name }}{{ end }}{{ template "mutate" . }}{{ mergeOverwrite .session .vars }}`,
	} {
		if err := validateGoTemplateSyntax(source); err == nil {
			t.Errorf("accepted trusted field in mutating template: %s", source)
		}
	}
}
