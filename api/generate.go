package api

import (
	"fmt"
	"regexp"
	"syscall"

	"golang.org/x/tools/imports"

	"github.com/99designs/gqlgen/codegen"
	"github.com/99designs/gqlgen/codegen/config"
	"github.com/99designs/gqlgen/plugin"
	"github.com/99designs/gqlgen/plugin/federation"
	"github.com/99designs/gqlgen/plugin/modelgen"
	"github.com/99designs/gqlgen/plugin/resolvergen"
)

var (
	urlRegex     = regexp.MustCompile(`(?s)@link.*\(.*url:\s*?"(.*?)"[^)]+\)`) // regex to grab the url of a link directive, should it exist
	versionRegex = regexp.MustCompile(`v(\d+).(\d+)$`)                         // regex to grab the version number from a url
)

func Generate(cfg *config.Config, option ...Option) error {
	_ = syscall.Unlink(cfg.Exec.Filename)
	if cfg.Model.IsDefined() {
		_ = syscall.Unlink(cfg.Model.Filename)
	}

	plugins := []plugin.Plugin{}
	if cfg.Model.IsDefined() {
		plugins = append(plugins, modelgen.New())
	}
	plugins = append(plugins, resolvergen.New())
	if cfg.Federation.IsDefined() {
		if cfg.Federation.Version == 0 { // default to using the user's choice of version, but if unset, try to sort out which federation version to use
			// check the sources, and if one is marked as federation v2, we mark the entirety to be generated using that format
			for _, v := range cfg.Sources {
				cfg.Federation.Version = 1
				urlString := urlRegex.FindStringSubmatch(v.Input)
				// e.g. urlString[1] == "https://specs.apollo.dev/federation/v2.7"
				if urlString != nil {
					matches := versionRegex.FindStringSubmatch(urlString[1])
					if matches[1] == "2" {
						cfg.Federation.Version = 2
						break
					}
				}
			}
		}
		federationPlugin, err := federation.New(cfg.Federation.Version, cfg)
		if err != nil {
			return fmt.Errorf("failed to construct the Federation plugin: %w", err)
		}
		plugins = append([]plugin.Plugin{federationPlugin}, plugins...)
	}

	for _, o := range option {
		o(cfg, &plugins)
	}

	if cfg.LocalPrefix != "" {
		imports.LocalPrefix = cfg.LocalPrefix
	}

	for _, p := range plugins {
		if inj, ok := p.(plugin.EarlySourceInjector); ok {
			if s := inj.InjectSourceEarly(); s != nil {
				cfg.Sources = append(cfg.Sources, s)
			}
		}
		if inj, ok := p.(plugin.EarlySourcesInjector); ok {
			s, err := inj.InjectSourcesEarly()
			if err != nil {
				return fmt.Errorf("%s: %w", p.Name(), err)
			}
			cfg.Sources = append(cfg.Sources, s...)
		}
	}

	if err := cfg.LoadSchema(); err != nil {
		return fmt.Errorf("failed to load schema: %w", err)
	}

	for _, p := range plugins {
		if inj, ok := p.(plugin.LateSourceInjector); ok {
			if s := inj.InjectSourceLate(cfg.Schema); s != nil {
				cfg.Sources = append(cfg.Sources, s)
			}
		}
		if inj, ok := p.(plugin.LateSourcesInjector); ok {
			s, err := inj.InjectSourcesLate(cfg.Schema)
			if err != nil {
				return fmt.Errorf("%s: %w", p.Name(), err)
			}
			cfg.Sources = append(cfg.Sources, s...)
		}
	}

	// LoadSchema again now we have everything
	if err := cfg.LoadSchema(); err != nil {
		return fmt.Errorf("failed to load schema: %w", err)
	}

	if err := cfg.Init(); err != nil {
		return fmt.Errorf("generating core failed: %w", err)
	}

	for _, p := range plugins {
		if mut, ok := p.(plugin.SchemaMutator); ok {
			err := mut.MutateSchema(cfg.Schema)
			if err != nil {
				return fmt.Errorf("%s: %w", p.Name(), err)
			}
		}
	}

	for _, p := range plugins {
		if mut, ok := p.(plugin.ConfigMutator); ok {
			err := mut.MutateConfig(cfg)
			if err != nil {
				return fmt.Errorf("%s: %w", p.Name(), err)
			}
		}
	}
	// Merge again now that the generated models have been injected into the typemap
	dataPlugins := make([]any, len(plugins))
	for index := range plugins {
		dataPlugins[index] = plugins[index]
	}
	data, err := codegen.BuildData(cfg, dataPlugins...)
	if err != nil {
		return fmt.Errorf("merging type systems failed: %w", err)
	}

	for _, p := range plugins {
		if mut, ok := p.(plugin.CodeGenerator); ok {
			err := mut.GenerateCode(data)
			if err != nil {
				return fmt.Errorf("%s: %w", p.Name(), err)
			}
		}
	}

	if err = codegen.GenerateCode(data); err != nil {
		return fmt.Errorf("generating core failed: %w", err)
	}

	if !cfg.SkipModTidy {
		if err = cfg.Packages.ModTidy(); err != nil {
			return fmt.Errorf("tidy failed: %w", err)
		}
	}
	if !cfg.SkipValidation {
		if err := validate(cfg); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}
	}

	return nil
}

func validate(cfg *config.Config) error {
	roots := []string{cfg.Exec.ImportPath()}
	if cfg.Model.IsDefined() {
		roots = append(roots, cfg.Model.ImportPath())
	}

	if cfg.Resolver.IsDefined() {
		roots = append(roots, cfg.Resolver.ImportPath())
	}

	cfg.Packages.LoadAll(roots...)
	errs := cfg.Packages.Errors()
	if len(errs) > 0 {
		return errs
	}
	return nil
}
