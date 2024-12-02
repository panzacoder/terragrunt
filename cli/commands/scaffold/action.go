package scaffold

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/gruntwork-io/terragrunt/config"
	"github.com/gruntwork-io/terragrunt/shell"

	"github.com/gruntwork-io/terragrunt/terraform"

	"github.com/gruntwork-io/terragrunt/cli/commands/hclfmt"
	"github.com/gruntwork-io/terragrunt/util"

	boilerplate_options "github.com/gruntwork-io/boilerplate/options"
	"github.com/gruntwork-io/boilerplate/templates"
	"github.com/gruntwork-io/boilerplate/variables"
	"github.com/gruntwork-io/terragrunt/internal/errors"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terratest/modules/files"
	"github.com/hashicorp/go-getter/v2"
)

const (
	sourceURLTypeHTTPS = "git-https"
	sourceURLTypeGit   = "git-ssh"
	sourceGitSSHUser   = "git"

	sourceURLTypeVar    = "SourceUrlType"
	sourceGitSSHUserVar = "SourceGitSshUser"
	refVar              = "Ref"
	// refParam - ?ref param from url
	refParam = "ref"

	moduleURLPattern = `(?:git|hg|s3|gcs)::([^:]+)://([^/]+)(/.*)`
	moduleURLParts   = 4

	// TODO: Make the root configuration file name configurable
	DefaultBoilerplateConfig = `
variables:
  - name: EnableRootInclude
    description: Should include root module
    type: bool
    default: true
`
	DefaultTerragruntTemplate = `
# This is a Terragrunt module generated by boilerplate.
terraform {
  source = "{{ .sourceUrl }}"
}
{{ if .EnableRootInclude }}
include "root" {
  path = find_in_parent_folders()
}
{{ end }}
inputs = {
  # --------------------------------------------------------------------------------------------------------------------
  # Required input variables
  # --------------------------------------------------------------------------------------------------------------------
  {{ range .requiredVariables }}
  {{- if eq 1 (regexSplit "\n" .Description -1 | len ) }}
  # Description: {{ .Description }}
  {{- else }}
  # Description:
    {{- range $line := regexSplit "\n" .Description -1 }}
    # {{ $line | indent 2 }}
    {{- end }}
  {{- end }}
  # Type: {{ .Type }}
  {{ .Name }} = {{ .DefaultValuePlaceholder }}  # TODO: fill in value
  {{ end }}

  # --------------------------------------------------------------------------------------------------------------------
  # Optional input variables
  # Uncomment the ones you wish to set
  # --------------------------------------------------------------------------------------------------------------------
  {{ range .optionalVariables }}
  {{- if eq 1 (regexSplit "\n" .Description -1 | len ) }}
  # Description: {{ .Description }}
  {{- else }}
  # Description:
    {{- range $line := regexSplit "\n" .Description -1 }}
    # {{ $line | indent 2 }}
    {{- end }}
  {{- end }}
  # Type: {{ .Type }}
  # {{ .Name }} = {{ .DefaultValue }}
  {{ end }}
}
`
)

var moduleURLRegex = regexp.MustCompile(moduleURLPattern)

func Run(ctx context.Context, opts *options.TerragruntOptions, moduleURL, templateURL string) error {
	// download remote repo to local
	var dirsToClean []string
	// clean all temp dirs
	defer func() {
		for _, dir := range dirsToClean {
			if err := os.RemoveAll(dir); err != nil {
				opts.Logger.Warnf("Failed to clean up dir %s: %v", dir, err)
			}
		}
	}()

	// scaffold only in empty directories
	if empty, err := util.IsDirectoryEmpty(opts.WorkingDir); !empty || err != nil {
		if err != nil {
			return err
		}

		opts.Logger.Warnf("The working directory %s is not empty.", opts.WorkingDir)
	}

	if moduleURL == "" {
		return errors.New(NoModuleURLPassed{})
	}

	// create temporary directory where to download module
	tempDir, err := os.MkdirTemp("", "scaffold")
	if err != nil {
		return errors.New(err)
	}

	dirsToClean = append(dirsToClean, tempDir)

	// prepare variables
	vars, err := variables.ParseVars(opts.ScaffoldVars, opts.ScaffoldVarFiles)
	if err != nil {
		return errors.New(err)
	}

	// parse module url
	moduleURL, err = parseModuleURL(ctx, opts, vars, moduleURL)
	if err != nil {
		return errors.New(err)
	}

	opts.Logger.Infof("Scaffolding a new Terragrunt module %s to %s", moduleURL, opts.WorkingDir)

	if _, err := getter.GetAny(ctx, tempDir, moduleURL); err != nil {
		return errors.New(err)
	}

	// extract variables from downloaded module
	requiredVariables, optionalVariables, err := parseVariables(opts, tempDir)
	if err != nil {
		return errors.New(err)
	}

	opts.Logger.Debugf("Parsed %d required variables and %d optional variables", len(requiredVariables), len(optionalVariables))

	// prepare boilerplate files to render Terragrunt files
	boilerplateDir, err := prepareBoilerplateFiles(ctx, opts, templateURL, tempDir)
	if err != nil {
		return errors.New(err)
	}

	// add additional variables
	vars["requiredVariables"] = requiredVariables
	vars["optionalVariables"] = optionalVariables

	vars["sourceUrl"] = moduleURL

	opts.Logger.Infof("Running boilerplate generation to %s", opts.WorkingDir)
	boilerplateOpts := &boilerplate_options.BoilerplateOptions{
		OutputFolder:    opts.WorkingDir,
		OnMissingKey:    boilerplate_options.DefaultMissingKeyAction,
		OnMissingConfig: boilerplate_options.DefaultMissingConfigAction,
		Vars:            vars,
		DisableShell:    true,
		DisableHooks:    true,
		NonInteractive:  opts.NonInteractive,
		TemplateFolder:  boilerplateDir,
	}

	emptyDep := variables.Dependency{}
	if err := templates.ProcessTemplate(boilerplateOpts, boilerplateOpts, emptyDep); err != nil {
		return errors.New(err)
	}

	opts.Logger.Infof("Running fmt on generated code %s", opts.WorkingDir)

	if err := hclfmt.Run(opts); err != nil {
		return errors.New(err)
	}

	opts.Logger.Info("Scaffolding completed")

	return nil
}

// prepareBoilerplateFiles prepares boilerplate files.
func prepareBoilerplateFiles(ctx context.Context, opts *options.TerragruntOptions, templateURL string, tempDir string) (string, error) {
	// identify template url
	templateDir := ""

	if templateURL != "" {
		// process template url if was passed
		parsedTemplateURL, err := terraform.ToSourceURL(templateURL, tempDir)
		if err != nil {
			return "", errors.New(err)
		}

		parsedTemplateURL, err = rewriteTemplateURL(ctx, opts, parsedTemplateURL)
		if err != nil {
			return "", errors.New(err)
		}
		// regenerate template url with all changes
		templateURL = parsedTemplateURL.String()

		// prepare temporary directory for template
		templateDir, err = os.MkdirTemp("", "template")
		if err != nil {
			return "", errors.New(err)
		}

		// downloading template
		opts.Logger.Infof("Using template from %s", templateURL)

		if _, err := getter.GetAny(ctx, templateDir, templateURL); err != nil {
			return "", errors.New(err)
		}
	}
	// prepare boilerplate dir
	boilerplateDir := util.JoinPath(tempDir, util.DefaultBoilerplateDir)
	// use template dir as boilerplate dir
	if templateDir != "" {
		boilerplateDir = templateDir
	}

	// if boilerplate dir is not found, create one with default template
	if !files.IsExistingDir(boilerplateDir) {
		// no default boilerplate dir, create one
		defaultTempDir, err := os.MkdirTemp("", "boilerplate")
		if err != nil {
			return "", errors.New(err)
		}

		boilerplateDir = defaultTempDir

		const ownerWriteGlobalReadPerms = 0644
		if err := os.WriteFile(util.JoinPath(boilerplateDir, "terragrunt.hcl"), []byte(DefaultTerragruntTemplate), ownerWriteGlobalReadPerms); err != nil {
			return "", errors.New(err)
		}

		if err := os.WriteFile(util.JoinPath(boilerplateDir, "boilerplate.yml"), []byte(DefaultBoilerplateConfig), ownerWriteGlobalReadPerms); err != nil {
			return "", errors.New(err)
		}
	}

	return boilerplateDir, nil
}

// parseVariables - parse variables from tf files.
func parseVariables(opts *options.TerragruntOptions, moduleDir string) ([]*config.ParsedVariable, []*config.ParsedVariable, error) {
	inputs, err := config.ParseVariables(opts, moduleDir)
	if err != nil {
		return nil, nil, errors.New(err)
	}

	// separate variables that require value and with default value
	var (
		requiredVariables []*config.ParsedVariable
		optionalVariables []*config.ParsedVariable
	)

	for _, value := range inputs {
		if value.DefaultValue == "" {
			requiredVariables = append(requiredVariables, value)
		} else {
			optionalVariables = append(optionalVariables, value)
		}
	}

	return requiredVariables, optionalVariables, nil
}

// parseModuleURL - parse module url and rewrite it if required
func parseModuleURL(ctx context.Context, opts *options.TerragruntOptions, vars map[string]interface{}, moduleURL string) (string, error) {
	parsedModuleURL, err := terraform.ToSourceURL(moduleURL, opts.WorkingDir)
	if err != nil {
		return "", errors.New(err)
	}

	moduleURL = parsedModuleURL.String()

	// rewrite module url, if required
	parsedModuleURL, err = rewriteModuleURL(opts, vars, moduleURL)
	if err != nil {
		return "", errors.New(err)
	}

	// add ref to module url, if required
	parsedModuleURL, err = addRefToModuleURL(ctx, opts, parsedModuleURL, vars)
	if err != nil {
		return "", errors.New(err)
	}

	// regenerate module url with all changes
	return parsedModuleURL.String(), nil
}

// rewriteModuleURL rewrites module url to git ssh if required
// github.com/gruntwork-io/terragrunt.git//test/fixtures/inputs => git::https://github.com/gruntwork-io/terragrunt.git//test/fixtures/inputs
func rewriteModuleURL(opts *options.TerragruntOptions, vars map[string]interface{}, moduleURL string) (*url.URL, error) {
	var updatedModuleURL = moduleURL

	sourceURLType := sourceURLTypeHTTPS
	if value, found := vars[sourceURLTypeVar]; found {
		sourceURLType = fmt.Sprintf("%s", value)
	}

	// expand module url
	parsedValue, err := parseURL(opts, moduleURL)
	if err != nil {
		opts.Logger.Warnf("Failed to parse module url %s", moduleURL)

		parsedModuleURL, err := terraform.ToSourceURL(updatedModuleURL, opts.WorkingDir)
		if err != nil {
			return nil, errors.New(err)
		}

		return parsedModuleURL, nil
	}
	// try to rewrite module url if is https and is requested to be git
	// git::https://github.com/gruntwork-io/terragrunt.git//test/fixtures/inputs => git::ssh://git@github.com/gruntwork-io/terragrunt.git//test/fixtures/inputs
	if parsedValue.scheme == "https" && sourceURLType == sourceURLTypeGit {
		gitUser := sourceGitSSHUser
		if value, found := vars[sourceGitSSHUserVar]; found {
			gitUser = fmt.Sprintf("%s", value)
		}

		path := strings.TrimPrefix(parsedValue.path, "/")
		updatedModuleURL = fmt.Sprintf("%s@%s:%s", gitUser, parsedValue.host, path)
	}

	// persist changes in url.URL
	parsedModuleURL, err := terraform.ToSourceURL(updatedModuleURL, opts.WorkingDir)
	if err != nil {
		return nil, errors.New(err)
	}

	return parsedModuleURL, nil
}

// rewriteTemplateURL rewrites template url with reference to tag
// github.com/denis256/terragrunt-tests.git//scaffold/base-template => github.com/denis256/terragrunt-tests.git//scaffold/base-template?ref=v0.53.8
func rewriteTemplateURL(ctx context.Context, opts *options.TerragruntOptions, parsedTemplateURL *url.URL) (*url.URL, error) {
	var (
		updatedTemplateURL = parsedTemplateURL
		templateParams     = updatedTemplateURL.Query()
	)

	ref := templateParams.Get(refParam)
	if ref == "" {
		rootSourceURL, _, err := terraform.SplitSourceURL(updatedTemplateURL, opts.Logger)
		if err != nil {
			return nil, errors.New(err)
		}

		tag, err := shell.GitLastReleaseTag(ctx, opts, rootSourceURL)
		if err != nil || tag == "" {
			opts.Logger.Warnf("Failed to find last release tag for URL %s, so will not add a ref param to the URL", rootSourceURL)
		} else {
			templateParams.Add(refParam, tag)
			updatedTemplateURL.RawQuery = templateParams.Encode()
		}
	}

	return updatedTemplateURL, nil
}

// addRefToModuleURL adds ref to module url if is passed through variables or find it from git tags
func addRefToModuleURL(ctx context.Context, opts *options.TerragruntOptions, parsedModuleURL *url.URL, vars map[string]interface{}) (*url.URL, error) {
	var moduleURL = parsedModuleURL
	// append ref to source url, if is passed through variables or find it from git tags
	params := moduleURL.Query()

	refReplacement, refVarPassed := vars[refVar]
	if refVarPassed {
		params.Set(refParam, fmt.Sprintf("%s", refReplacement))
		moduleURL.RawQuery = params.Encode()
	}

	ref := params.Get(refParam)
	if ref == "" {
		// if ref is not passed, find last release tag
		// git::https://github.com/gruntwork-io/terragrunt.git//test/fixtures/inputs => git::https://github.com/gruntwork-io/terragrunt.git//test/fixtures/inputs?ref=v0.53.8
		rootSourceURL, _, err := terraform.SplitSourceURL(moduleURL, opts.Logger)
		if err != nil {
			return nil, errors.New(err)
		}

		tag, err := shell.GitLastReleaseTag(ctx, opts, rootSourceURL)
		if err != nil || tag == "" {
			opts.Logger.Warnf("Failed to find last release tag for %s", rootSourceURL)
		} else {
			params.Add(refParam, tag)
			moduleURL.RawQuery = params.Encode()
		}
	}

	return moduleURL, nil
}

// parseURL parses module url to scheme, host and path
func parseURL(opts *options.TerragruntOptions, moduleURL string) (*parsedURL, error) {
	matches := moduleURLRegex.FindStringSubmatch(moduleURL)
	if len(matches) != moduleURLParts {
		opts.Logger.Warnf("Failed to parse url %s", moduleURL)
		return nil, failedToParseURLError{}
	}

	return &parsedURL{
		scheme: matches[1],
		host:   matches[2],
		path:   matches[3],
	}, nil
}

type parsedURL struct {
	scheme string
	host   string
	path   string
}

type failedToParseURLError struct {
}

func (err failedToParseURLError) Error() string {
	return "Failed to parse Url."
}

type NoModuleURLPassed struct {
}

func (err NoModuleURLPassed) Error() string {
	return "No module URL passed."
}
