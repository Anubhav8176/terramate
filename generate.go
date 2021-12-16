// Copyright 2021 Mineiros GmbH
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

package terramate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tfhcl "github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/madlambda/spells/errutil"
	"github.com/mineiros-io/terramate/config"
	"github.com/mineiros-io/terramate/hcl"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
)

const (
	// GeneratedTfFilename is the name of the terramate generated tf file.
	GeneratedTfFilename = "_gen_terramate.tm.tf"

	// GeneratedCodeHeader is the header added on all generated files.
	GeneratedCodeHeader = "// GENERATED BY TERRAMATE: DO NOT EDIT\n\n"
)

// Generate will walk all the directories starting from basedir generating
// code for any stack it finds as it goes along
//
// It will return an error if it finds any invalid terramate configuration files
// of if it can't generate the files properly for some reason.
//
// The provided basedir must be an absolute path to a directory.
func Generate(basedir string) error {
	if !filepath.IsAbs(basedir) {
		return fmt.Errorf("basedir %q must be an absolute path", basedir)
	}

	info, err := os.Lstat(basedir)
	if err != nil {
		return fmt.Errorf("checking basedir %q: %v", basedir, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("basedir %q is not a directory", basedir)
	}

	stackEntries, err := ListStacks(basedir)
	if err != nil {
		return fmt.Errorf("listing stack: %w", err)
	}

	metadata, err := LoadMetadata(basedir)
	if err != nil {
		return fmt.Errorf("loading metadata: %v", err)
	}

	var errs []error

	for _, entry := range stackEntries {
		// At the time the most intuitive way was to start from the stack
		// and go up until reaching the basedir, looking for a config.
		// Basically navigating from the order of precedence, since
		// more specific configuration overrides base configuration.
		// Not the most optimized way (re-parsing), we can improve later
		stackMetadata, ok := metadata.StackMetadata(entry.Stack.Dir)
		if !ok {
			errs = append(errs, fmt.Errorf("stack %q: no metadata found", entry.Stack.Dir))
			continue
		}

		evalctx, err := newHCLEvalContext(stackMetadata)
		if err != nil {
			errs = append(errs, fmt.Errorf("stack %q: building eval ctx: %v", entry.Stack.Dir, err))
			continue
		}

		tfcode, err := generateStackConfig(basedir, entry.Stack.Dir, evalctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("stack %q: %w", entry.Stack.Dir, err))
			continue
		}

		if tfcode == nil {
			continue
		}

		genfile := filepath.Join(entry.Stack.Dir, GeneratedTfFilename)
		errs = append(errs, os.WriteFile(genfile, tfcode, 0666))
	}

	if err := errutil.Chain(errs...); err != nil {
		return fmt.Errorf("failed to generate code: %w", err)
	}

	return nil
}

func generateStackConfig(basedir string, configdir string, evalctx *tfhcl.EvalContext) ([]byte, error) {
	if !strings.HasPrefix(configdir, basedir) {
		// check if we are outside of basedir, time to stop
		return nil, nil
	}

	configfile := filepath.Join(configdir, config.Filename)
	if _, err := os.Stat(configfile); err != nil {
		return generateStackConfig(basedir, filepath.Dir(configdir), evalctx)
	}

	config, err := os.ReadFile(configfile)
	if err != nil {
		return nil, fmt.Errorf("reading config: %v", err)
	}

	parsedConfig, err := hcl.Parse(configfile, config)
	if err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	parsed := parsedConfig.Terramate
	if parsed.Backend == nil {
		return generateStackConfig(basedir, filepath.Dir(configdir), evalctx)
	}

	gen := hclwrite.NewEmptyFile()
	rootBody := gen.Body()
	tfBlock := rootBody.AppendNewBlock("terraform", nil)
	tfBody := tfBlock.Body()
	backendBlock := tfBody.AppendNewBlock(parsed.Backend.Type, parsed.Backend.Labels)
	backendBody := backendBlock.Body()

	if err := copyBody(backendBody, parsed.Backend.Body, evalctx); err != nil {
		return nil, err
	}

	return append([]byte(GeneratedCodeHeader), gen.Bytes()...), nil
}

func copyBody(target *hclwrite.Body, src *hclsyntax.Body, evalctx *tfhcl.EvalContext) error {
	if src == nil || target == nil {
		return nil
	}

	// Avoid generating code with random attr order (map iteration is random)
	attrs := sortedAttributes(src.Attributes)

	for _, attr := range attrs {
		val, err := attr.Expr.Value(evalctx)
		if err != nil {
			return fmt.Errorf("parsing attribute %q: %v", attr.Name, err)
		}

		target.SetAttributeValue(attr.Name, val)
	}

	for _, block := range src.Blocks {
		targetBlock := target.AppendNewBlock(block.Type, block.Labels)
		targetBody := targetBlock.Body()

		if err := copyBody(targetBody, block.Body, evalctx); err != nil {
			return err
		}
	}

	return nil
}

func newHCLEvalContext(metadata StackMetadata) (*tfhcl.EvalContext, error) {
	vars, err := hclMapToCty(map[string]cty.Value{
		"name": cty.StringVal(metadata.Name),
		"path": cty.StringVal(metadata.Path),
	})

	if err != nil {
		return nil, err
	}

	return &tfhcl.EvalContext{
		Variables: map[string]cty.Value{"terramate": vars},
	}, nil
}

func hclMapToCty(m map[string]cty.Value) (cty.Value, error) {
	ctyTypes := map[string]cty.Type{}
	for key, value := range m {
		ctyTypes[key] = value.Type()
	}
	ctyObject := cty.Object(ctyTypes)
	ctyVal, err := gocty.ToCtyValue(m, ctyObject)
	if err != nil {
		return cty.Value{}, err
	}
	return ctyVal, nil
}

func sortedAttributes(attrs hclsyntax.Attributes) []*hclsyntax.Attribute {
	names := make([]string, 0, len(attrs))

	for name := range attrs {
		names = append(names, name)
	}

	sort.Strings(names)

	sorted := make([]*hclsyntax.Attribute, len(names))
	for i, name := range names {
		sorted[i] = attrs[name]
	}

	return sorted
}