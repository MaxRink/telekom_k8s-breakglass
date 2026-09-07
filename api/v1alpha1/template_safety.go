// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"text/template"
	"text/template/parse"
)

// ValidateTemplateOutput checks output actions without executing administrator
// code. Unknown values (including aliases and rebound dot) need a final scalar
// serializer. This deliberately does not infer safety through arbitrary Sprig
// functions: a decoder after a serializer can reconstruct YAML structure.
// A conservative context pass also requires serializers to occupy complete YAML
// scalar positions across text, branches, loops, and named-template calls.
func ValidateTemplateOutput(tmpl *template.Template) error {
	called := map[string]bool{}
	for _, definition := range tmpl.Templates() {
		if definition.Tree != nil {
			collectTemplateCalls(definition.Tree.Root, called)
		}
	}
	for _, definition := range tmpl.Templates() {
		if definition.Tree != nil {
			if err := validateOutputList(definition.Tree.Root, definition.Name() == tmpl.Name() && !called[definition.Name()]); err != nil {
				return err
			}
		}
	}
	if tmpl.Tree != nil {
		_, err := validateScalarContexts(tmpl, tmpl.Tree.Root, []scalarContext{{start: true}}, map[string]bool{tmpl.Name(): true})
		return err
	}
	return nil
}

func validateOutputList(list *parse.ListNode, rootDot bool) error {
	if list == nil {
		return nil
	}
	for _, node := range list.Nodes {
		switch n := node.(type) {
		case *parse.ActionNode:
			if len(n.Pipe.Decl) == 0 && !safeOutputPipe(n.Pipe, rootDot) {
				return fmt.Errorf("template output %s must end in yamlQuote, yamlSafe, quote, or k8sName; serialize the complete scalar", n.String())
			}
		case *parse.IfNode:
			if err := validateOutputList(n.List, rootDot); err != nil {
				return err
			}
			if err := validateOutputList(n.ElseList, rootDot); err != nil {
				return err
			}
		case *parse.WithNode:
			if err := validateOutputList(n.List, false); err != nil {
				return err
			}
			if err := validateOutputList(n.ElseList, rootDot); err != nil {
				return err
			}
		case *parse.RangeNode:
			if err := validateOutputList(n.List, false); err != nil {
				return err
			}
			if err := validateOutputList(n.ElseList, rootDot); err != nil {
				return err
			}
		}
	}
	return nil
}

func safeOutputPipe(pipe *parse.PipeNode, rootDot bool) bool {
	if len(pipe.Cmds) == 0 {
		return false
	}
	cmd := pipe.Cmds[len(pipe.Cmds)-1]
	if len(cmd.Args) == 0 {
		return false
	}
	if fn, ok := cmd.Args[0].(*parse.IdentifierNode); ok {
		switch fn.Ident {
		case "yamlQuote", "yamlSafe", "quote", "k8sName":
			return true
		// These functions return booleans or numbers, never arbitrary text.
		case "eq", "ne", "lt", "le", "gt", "ge", "not", "empty", "hasKey", "contains", "hasPrefix", "hasSuffix", "len", "int", "int64", "float64", "atoi", "add", "add1", "sub", "mul", "div", "mod", "max", "min", "ceil", "floor", "round":
			return true
		}
		return false
	}
	if len(pipe.Cmds) != 1 || len(cmd.Args) != 1 {
		return false
	}
	switch value := cmd.Args[0].(type) {
	case *parse.StringNode, *parse.NumberNode, *parse.BoolNode:
		return true // Administrator-authored literals, not requester data.
	case *parse.PipeNode:
		return safeOutputPipe(value, rootDot)
	case *parse.FieldNode:
		if !rootDot {
			return false
		}
		// Kubernetes identifiers are validated independently of template rendering.
		switch value.String() {
		case ".session.name", ".session.namespace", ".session.cluster", ".target.namespace", ".target.cluster", ".binding.name", ".binding.namespace":
			return true
		}
	}
	return false
}

func collectTemplateCalls(list *parse.ListNode, called map[string]bool) {
	if list == nil {
		return
	}
	for _, node := range list.Nodes {
		switch n := node.(type) {
		case *parse.TemplateNode:
			called[n.Name] = true
		case *parse.IfNode:
			collectTemplateCalls(n.List, called)
			collectTemplateCalls(n.ElseList, called)
		case *parse.WithNode:
			collectTemplateCalls(n.List, called)
			collectTemplateCalls(n.ElseList, called)
		case *parse.RangeNode:
			collectTemplateCalls(n.List, called)
			collectTemplateCalls(n.ElseList, called)
		}
	}
}
