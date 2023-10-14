// Copyright 2015 Google Inc. All rights reserved.
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

package android

// This is the primary location to write and read all configuration values and
// product variables necessary for soong_build's operation.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/google/blueprint"
	"github.com/google/blueprint/bootstrap"
	"github.com/google/blueprint/pathtools"
	"github.com/google/blueprint/proptools"

	"android/soong/android/soongconfig"
	"android/soong/bazel"
	"android/soong/remoteexec"
	"android/soong/starlark_fmt"
)

// Bool re-exports proptools.Bool for the android package.
var Bool = proptools.Bool

// String re-exports proptools.String for the android package.
var String = proptools.String

// StringDefault re-exports proptools.StringDefault for the android package.
var StringDefault = proptools.StringDefault

// FutureApiLevelInt is a placeholder constant for unreleased API levels.
const FutureApiLevelInt = 10000

// PrivateApiLevel represents the api level of SdkSpecPrivate (sdk_version: "")
// This api_level exists to differentiate user-provided "" from "current" sdk_version
// The differentiation is necessary to enable different validation rules for these two possible values.
var PrivateApiLevel = ApiLevel{
	value:     "current",             // The value is current since aidl expects `current` as the default (TestAidlFlagsWithMinSdkVersion)
	number:    FutureApiLevelInt + 1, // This is used to differentiate it from FutureApiLevel
	isPreview: true,
}

// FutureApiLevel represents unreleased API levels.
var FutureApiLevel = ApiLevel{
	value:     "current",
	number:    FutureApiLevelInt,
	isPreview: true,
}

// The product variables file name, containing product config from Kati.
const productVariablesFileName = "soong.variables"

// A Config object represents the entire build configuration for Android.
type Config struct {
	*config
}

type SoongBuildMode int

type CmdArgs struct {
	bootstrap.Args
	RunGoTests  bool
	OutDir      string
	SoongOutDir string

	SymlinkForestMarker string
	Bp2buildMarker      string
	BazelQueryViewDir   string
	BazelApiBp2buildDir string
	ModuleGraphFile     string
	ModuleActionsFile   string
	DocFile             string

	MultitreeBuild bool

	BazelMode                bool
	BazelModeDev             bool
	BazelModeStaging         bool
	BazelForceEnabledModules string

	UseBazelProxy bool

	BuildFromTextStub bool
}

// Build modes that soong_build can run as.
const (
	// Don't use bazel at all during module analysis.
	AnalysisNoBazel SoongBuildMode = iota

	// Symlink fores mode: merge two directory trees into a symlink forest
	SymlinkForest

	// Bp2build mode: Generate BUILD files from blueprint files and exit.
	Bp2build

	// Generate BUILD files which faithfully represent the dependency graph of
	// blueprint modules. Individual BUILD targets will not, however, faitfhully
	// express build semantics.
	GenerateQueryView

	// Generate BUILD files for API contributions to API surfaces
	ApiBp2build

	// Create a JSON representation of the module graph and exit.
	GenerateModuleGraph

	// Generate a documentation file for module type definitions and exit.
	GenerateDocFile

	// Use bazel during analysis of many allowlisted build modules. The allowlist
	// is considered a "developer mode" allowlist, as some modules may be
	// allowlisted on an experimental basis.
	BazelDevMode

	// Use bazel during analysis of a few allowlisted build modules. The allowlist
	// is considered "staging, as these are modules being prepared to be released
	// into prod mode shortly after.
	BazelStagingMode

	// Use bazel during analysis of build modules from an allowlist carefully
	// curated by the build team to be proven stable.
	BazelProdMode
)

// SoongOutDir returns the build output directory for the configuration.
func (c Config) SoongOutDir() string {
	return c.soongOutDir
}

func (c Config) OutDir() string {
	return c.outDir
}

func (c Config) RunGoTests() bool {
	return c.runGoTests
}

func (c Config) DebugCompilation() bool {
	return false // Never compile Go code in the main build for debugging
}

func (c Config) Subninjas() []string {
	return []string{}
}

func (c Config) PrimaryBuilderInvocations() []bootstrap.PrimaryBuilderInvocation {
	return []bootstrap.PrimaryBuilderInvocation{}
}

// RunningInsideUnitTest returns true if this code is being run as part of a Soong unit test.
func (c Config) RunningInsideUnitTest() bool {
	return c.config.TestProductVariables != nil
}

// MaxPageSizeSupported returns the max page size supported by the device. This
// value will define the ELF segment alignment for binaries (executables and
// shared libraries).
func (c Config) MaxPageSizeSupported() string {
	return String(c.config.productVariables.DeviceMaxPageSizeSupported)
}

// A DeviceConfig object represents the configuration for a particular device
// being built. For now there will only be one of these, but in the future there
// may be multiple devices being built.
type DeviceConfig struct {
	*deviceConfig
}

// VendorConfig represents the configuration for vendor-specific behavior.
type VendorConfig soongconfig.SoongConfig

// Definition of general build configuration for soong_build. Some of these
// product configuration values are read from Kati-generated soong.variables.
type config struct {
	// Options configurable with soong.variables
	productVariables productVariables

	// Only available on configs created by TestConfig
	TestProductVariables *productVariables

	// A specialized context object for Bazel/Soong mixed builds and migration
	// purposes.
	BazelContext BazelContext

	ProductVariablesFileName string

	// BuildOS stores the OsType for the OS that the build is running on.
	BuildOS OsType

	// BuildArch stores the ArchType for the CPU that the build is running on.
	BuildArch ArchType

	Targets                  map[OsType][]Target
	BuildOSTarget            Target // the Target for tools run on the build machine
	BuildOSCommonTarget      Target // the Target for common (java) tools run on the build machine
	AndroidCommonTarget      Target // the Target for common modules for the Android device
	AndroidFirstDeviceTarget Target // the first Target for modules for the Android device

	// multilibConflicts for an ArchType is true if there is earlier configured
	// device architecture with the same multilib value.
	multilibConflicts map[ArchType]bool

	deviceConfig *deviceConfig

	outDir         string // The output directory (usually out/)
	soongOutDir    string
	moduleListFile string // the path to the file which lists blueprint files to parse.

	runGoTests bool

	env       map[string]string
	envLock   sync.Mutex
	envDeps   map[string]string
	envFrozen bool

	// Changes behavior based on whether Kati runs after soong_build, or if soong_build
	// runs standalone.
	katiEnabled bool

	captureBuild      bool // true for tests, saves build parameters for each module
	ignoreEnvironment bool // true for tests, returns empty from all Getenv calls

	fs         pathtools.FileSystem
	mockBpList string

	BuildMode                      SoongBuildMode
	Bp2buildPackageConfig          Bp2BuildConversionAllowlist
	Bp2buildSoongConfigDefinitions soongconfig.Bp2BuildSoongConfigDefinitions

	// If MultitreeBuild is true then this is one inner tree of a multitree
	// build directed by the multitree orchestrator.
	MultitreeBuild bool

	// If testAllowNonExistentPaths is true then PathForSource and PathForModuleSrc won't error
	// in tests when a path doesn't exist.
	TestAllowNonExistentPaths bool

	// The list of files that when changed, must invalidate soong_build to
	// regenerate build.ninja.
	ninjaFileDepsSet sync.Map

	OncePer

	// These fields are only used for metrics collection. A module should be added
	// to these maps only if its implementation supports Bazel handling in mixed
	// builds. A module being in the "enabled" list indicates that there is a
	// variant of that module for which bazel-handling actually took place.
	// A module being in the "disabled" list indicates that there is a variant of
	// that module for which bazel-handling was denied.
	mixedBuildsLock           sync.Mutex
	mixedBuildEnabledModules  map[string]struct{}
	mixedBuildDisabledModules map[string]struct{}

	// These are modules to be built with Bazel beyond the allowlisted/build-mode
	// specified modules. They are passed via the command-line flag
	// "--bazel-force-enabled-modules"
	bazelForceEnabledModules map[string]struct{}

	// If true, for any requests to Bazel, communicate with a Bazel proxy using
	// unix sockets, instead of spawning Bazel as a subprocess.
	UseBazelProxy bool

	// If buildFromTextStub is true then the Java API stubs are
	// built from the signature text files, not the source Java files.
	buildFromTextStub bool
}

type deviceConfig struct {
	config *config
	OncePer
}

type jsonConfigurable interface {
	SetDefaultConfig()
}

func loadConfig(config *config) error {
	return loadFromConfigFile(&config.productVariables, absolutePath(config.ProductVariablesFileName))
}

// loadFromConfigFile loads and decodes configuration options from a JSON file
// in the current working directory.
func loadFromConfigFile(configurable *productVariables, filename string) error {
	// Try to open the file
	configFileReader, err := os.Open(filename)
	defer configFileReader.Close()
	if os.IsNotExist(err) {
		// Need to create a file, so that blueprint & ninja don't get in
		// a dependency tracking loop.
		// Make a file-configurable-options with defaults, write it out using
		// a json writer.
		configurable.SetDefaultConfig()
		err = saveToConfigFile(configurable, filename)
		if err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("config file: could not open %s: %s", filename, err.Error())
	} else {
		// Make a decoder for it
		jsonDecoder := json.NewDecoder(configFileReader)
		err = jsonDecoder.Decode(configurable)
		if err != nil {
			return fmt.Errorf("config file: %s did not parse correctly: %s", filename, err.Error())
		}
	}

	if Bool(configurable.GcovCoverage) && Bool(configurable.ClangCoverage) {
		return fmt.Errorf("GcovCoverage and ClangCoverage cannot both be set")
	}

	configurable.Native_coverage = proptools.BoolPtr(
		Bool(configurable.GcovCoverage) ||
			Bool(configurable.ClangCoverage))

	// when Platform_sdk_final is true (or PLATFORM_VERSION_CODENAME is REL), use Platform_sdk_version;
	// if false (pre-released version, for example), use Platform_sdk_codename.
	if Bool(configurable.Platform_sdk_final) {
		if configurable.Platform_sdk_version != nil {
			configurable.Platform_sdk_version_or_codename =
				proptools.StringPtr(strconv.Itoa(*(configurable.Platform_sdk_version)))
		} else {
			return fmt.Errorf("Platform_sdk_version cannot be pointed by a NULL pointer")
		}
	} else {
		configurable.Platform_sdk_version_or_codename =
			proptools.StringPtr(String(configurable.Platform_sdk_codename))
	}

	return saveToBazelConfigFile(configurable, filepath.Dir(filename))
}

// atomically writes the config file in case two copies of soong_build are running simultaneously
// (for example, docs generation and ninja manifest generation)
func saveToConfigFile(config *productVariables, filename string) error {
	data, err := json.MarshalIndent(&config, "", "    ")
	if err != nil {
		return fmt.Errorf("cannot marshal config data: %s", err.Error())
	}

	f, err := os.CreateTemp(filepath.Dir(filename), "config")
	if err != nil {
		return fmt.Errorf("cannot create empty config file %s: %s", filename, err.Error())
	}
	defer os.Remove(f.Name())
	defer f.Close()

	_, err = f.Write(data)
	if err != nil {
		return fmt.Errorf("default config file: %s could not be written: %s", filename, err.Error())
	}

	_, err = f.WriteString("\n")
	if err != nil {
		return fmt.Errorf("default config file: %s could not be written: %s", filename, err.Error())
	}

	f.Close()
	os.Rename(f.Name(), filename)

	return nil
}

func saveToBazelConfigFile(config *productVariables, outDir string) error {
	dir := filepath.Join(outDir, bazel.SoongInjectionDirName, "product_config")
	err := createDirIfNonexistent(dir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("Could not create dir %s: %s", dir, err)
	}

	nonArchVariantProductVariables := []string{}
	archVariantProductVariables := []string{}
	p := variableProperties{}
	t := reflect.TypeOf(p.Product_variables)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		nonArchVariantProductVariables = append(nonArchVariantProductVariables, strings.ToLower(f.Name))
		if proptools.HasTag(f, "android", "arch_variant") {
			archVariantProductVariables = append(archVariantProductVariables, strings.ToLower(f.Name))
		}
	}

	nonArchVariantProductVariablesJson := starlark_fmt.PrintStringList(nonArchVariantProductVariables, 0)
	if err != nil {
		return fmt.Errorf("cannot marshal product variable data: %s", err.Error())
	}

	archVariantProductVariablesJson := starlark_fmt.PrintStringList(archVariantProductVariables, 0)
	if err != nil {
		return fmt.Errorf("cannot marshal arch variant product variable data: %s", err.Error())
	}

	configJson, err := json.MarshalIndent(&config, "", "    ")
	if err != nil {
		return fmt.Errorf("cannot marshal config data: %s", err.Error())
	}
	// The backslashes need to be escaped because this text is going to be put
	// inside a Starlark string literal.
	configJson = bytes.ReplaceAll(configJson, []byte("\\"), []byte("\\\\"))

	bzl := []string{
		bazel.GeneratedBazelFileWarning,
		fmt.Sprintf(`_product_vars = json.decode("""%s""")`, configJson),
		fmt.Sprintf(`_product_var_constraints = %s`, nonArchVariantProductVariablesJson),
		fmt.Sprintf(`_arch_variant_product_var_constraints = %s`, archVariantProductVariablesJson),
		"\n", `
product_vars = _product_vars

# TODO(b/269577299) Remove these when everything switches over to loading them from product_variable_constants.bzl
product_var_constraints = _product_var_constraints
arch_variant_product_var_constraints = _arch_variant_product_var_constraints
`,
	}
	err = pathtools.WriteFileIfChanged(filepath.Join(dir, "product_variables.bzl"),
		[]byte(strings.Join(bzl, "\n")), 0644)
	if err != nil {
		return fmt.Errorf("Could not write .bzl config file %s", err)
	}
	err = pathtools.WriteFileIfChanged(filepath.Join(dir, "product_variable_constants.bzl"), []byte(fmt.Sprintf(`
product_var_constraints = %s
arch_variant_product_var_constraints = %s
`, nonArchVariantProductVariablesJson, archVariantProductVariablesJson)), 0644)
	if err != nil {
		return fmt.Errorf("Could not write .bzl config file %s", err)
	}
	err = pathtools.WriteFileIfChanged(filepath.Join(dir, "BUILD"),
		[]byte(bazel.GeneratedBazelFileWarning), 0644)
	if err != nil {
		return fmt.Errorf("Could not write BUILD config file %s", err)
	}

	return nil
}

// NullConfig returns a mostly empty Config for use by standalone tools like dexpreopt_gen that
// use the android package.
func NullConfig(outDir, soongOutDir string) Config {
	return Config{
		config: &config{
			outDir:      outDir,
			soongOutDir: soongOutDir,
			fs:          pathtools.OsFs,
		},
	}
}

// NewConfig creates a new Config object. The srcDir argument specifies the path
// to the root source directory. It also loads the config file, if found.
func NewConfig(cmdArgs CmdArgs, availableEnv map[string]string) (Config, error) {
	// Make a config with default options.
	config := &config{
		ProductVariablesFileName: filepath.Join(cmdArgs.SoongOutDir, productVariablesFileName),

		env: availableEnv,

		outDir:            cmdArgs.OutDir,
		soongOutDir:       cmdArgs.SoongOutDir,
		runGoTests:        cmdArgs.RunGoTests,
		multilibConflicts: make(map[ArchType]bool),

		moduleListFile:            cmdArgs.ModuleListFile,
		fs:                        pathtools.NewOsFs(absSrcDir),
		mixedBuildDisabledModules: make(map[string]struct{}),
		mixedBuildEnabledModules:  make(map[string]struct{}),
		bazelForceEnabledModules:  make(map[string]struct{}),

		MultitreeBuild: cmdArgs.MultitreeBuild,
		UseBazelProxy:  cmdArgs.UseBazelProxy,

		buildFromTextStub: cmdArgs.BuildFromTextStub,
	}

	config.deviceConfig = &deviceConfig{
		config: config,
	}

	// Soundness check of the build and source directories. This won't catch strange
	// configurations with symlinks, but at least checks the obvious case.
	absBuildDir, err := filepath.Abs(cmdArgs.SoongOutDir)
	if err != nil {
		return Config{}, err
	}

	absSrcDir, err := filepath.Abs(".")
	if err != nil {
		return Config{}, err
	}

	if strings.HasPrefix(absSrcDir, absBuildDir) {
		return Config{}, fmt.Errorf("Build dir must not contain source directory")
	}

	// Load any configurable options from the configuration file
	err = loadConfig(config)
	if err != nil {
		return Config{}, err
	}

	KatiEnabledMarkerFile := filepath.Join(cmdArgs.SoongOutDir, ".soong.kati_enabled")
	if _, err := os.Stat(absolutePath(KatiEnabledMarkerFile)); err == nil {
		config.katiEnabled = true
	}

	determineBuildOS(config)

	// Sets up the map of target OSes to the finer grained compilation targets
	// that are configured from the product variables.
	targets, err := decodeTargetProductVariables(config)
	if err != nil {
		return Config{}, err
	}

	// Make the CommonOS OsType available for all products.
	targets[CommonOS] = []Target{commonTargetMap[CommonOS.Name]}

	var archConfig []archConfig
	if config.NdkAbis() {
		archConfig = getNdkAbisConfig()
	} else if config.AmlAbis() {
		archConfig = getAmlAbisConfig()
	}

	if archConfig != nil {
		androidTargets, err := decodeAndroidArchSettings(archConfig)
		if err != nil {
			return Config{}, err
		}
		targets[Android] = androidTargets
	}

	multilib := make(map[string]bool)
	for _, target := range targets[Android] {
		if seen := multilib[target.Arch.ArchType.Multilib]; seen {
			config.multilibConflicts[target.Arch.ArchType] = true
		}
		multilib[target.Arch.ArchType.Multilib] = true
	}

	// Map of OS to compilation targets.
	config.Targets = targets

	// Compilation targets for host tools.
	config.BuildOSTarget = config.Targets[config.BuildOS][0]
	config.BuildOSCommonTarget = getCommonTargets(config.Targets[config.BuildOS])[0]

	// Compilation targets for Android.
	if len(config.Targets[Android]) > 0 {
		config.AndroidCommonTarget = getCommonTargets(config.Targets[Android])[0]
		config.AndroidFirstDeviceTarget = FirstTarget(config.Targets[Android], "lib64", "lib32")[0]
	}

	setBuildMode := func(arg string, mode SoongBuildMode) {
		if arg != "" {
			if config.BuildMode != AnalysisNoBazel {
				fmt.Fprintf(os.Stderr, "buildMode is already set, illegal argument: %s", arg)
				os.Exit(1)
			}
			config.BuildMode = mode
		}
	}
	setBazelMode := func(arg bool, argName string, mode SoongBuildMode) {
		if arg {
			if config.BuildMode != AnalysisNoBazel {
				fmt.Fprintf(os.Stderr, "buildMode is already set, illegal argument: %s", argName)
				os.Exit(1)
			}
			config.BuildMode = mode
		}
	}
	setBuildMode(cmdArgs.SymlinkForestMarker, SymlinkForest)
	setBuildMode(cmdArgs.Bp2buildMarker, Bp2build)
	setBuildMode(cmdArgs.BazelQueryViewDir, GenerateQueryView)
	setBuildMode(cmdArgs.BazelApiBp2buildDir, ApiBp2build)
	setBuildMode(cmdArgs.ModuleGraphFile, GenerateModuleGraph)
	setBuildMode(cmdArgs.DocFile, GenerateDocFile)
	setBazelMode(cmdArgs.BazelModeDev, "--bazel-mode-dev", BazelDevMode)
	setBazelMode(cmdArgs.BazelMode, "--bazel-mode", BazelProdMode)
	setBazelMode(cmdArgs.BazelModeStaging, "--bazel-mode-staging", BazelStagingMode)

	for _, module := range strings.Split(cmdArgs.BazelForceEnabledModules, ",") {
		config.bazelForceEnabledModules[module] = struct{}{}
	}
	config.BazelContext, err = NewBazelContext(config)
	config.Bp2buildPackageConfig = GetBp2BuildAllowList()

	return Config{config}, err
}

// mockFileSystem replaces all reads with accesses to the provided map of
// filenames to contents stored as a byte slice.
func (c *config) mockFileSystem(bp string, fs map[string][]byte) {
	mockFS := map[string][]byte{}

	if _, exists := mockFS["Android.bp"]; !exists {
		mockFS["Android.bp"] = []byte(bp)
	}

	for k, v := range fs {
		mockFS[k] = v
	}

	// no module list file specified; find every file named Blueprints or Android.bp
	pathsToParse := []string{}
	for candidate := range mockFS {
		base := filepath.Base(candidate)
		if base == "Android.bp" {
			pathsToParse = append(pathsToParse, candidate)
		}
	}
	if len(pathsToParse) < 1 {
		panic(fmt.Sprintf("No Blueprint or Android.bp files found in mock filesystem: %v\n", mockFS))
	}
	mockFS[blueprint.MockModuleListFile] = []byte(strings.Join(pathsToParse, "\n"))

	c.fs = pathtools.MockFs(mockFS)
	c.mockBpList = blueprint.MockModuleListFile
}

// TODO(b/265062549): Add a field to our collected (and uploaded) metrics which
// describes a reason that we fell back to non-mixed builds.
// Returns true if "Bazel builds" is enabled. In this mode, part of build
// analysis is handled by Bazel.
func (c *config) IsMixedBuildsEnabled() bool {
	globalMixedBuildsSupport := c.Once(OnceKey{"globalMixedBuildsSupport"}, func() interface{} {
		if c.productVariables.DeviceArch != nil && *c.productVariables.DeviceArch == "riscv64" {
			return false
		}
		if c.IsEnvTrue("GLOBAL_THINLTO") {
			return false
		}
		if len(c.productVariables.SanitizeHost) > 0 {
			return false
		}
		if len(c.productVariables.SanitizeDevice) > 0 {
			return false
		}
		if len(c.productVariables.SanitizeDeviceDiag) > 0 {
			return false
		}
		if len(c.productVariables.SanitizeDeviceArch) > 0 {
			return false
		}
		return true
	}).(bool)

	bazelModeEnabled := c.BuildMode == BazelProdMode || c.BuildMode == BazelDevMode || c.BuildMode == BazelStagingMode
	return globalMixedBuildsSupport && bazelModeEnabled
}

func (c *config) SetAllowMissingDependencies() {
	c.productVariables.Allow_missing_dependencies = proptools.BoolPtr(true)
}

// BlueprintToolLocation returns the directory containing build system tools
// from Blueprint, like soong_zip and merge_zips.
func (c *config) HostToolDir() string {
	if c.KatiEnabled() {
		return filepath.Join(c.outDir, "host", c.PrebuiltOS(), "bin")
	} else {
		return filepath.Join(c.soongOutDir, "host", c.PrebuiltOS(), "bin")
	}
}

func (c *config) HostToolPath(ctx PathContext, tool string) Path {
	path := pathForInstall(ctx, ctx.Config().BuildOS, ctx.Config().BuildArch, "bin", false, tool)
	return path
}

func (c *config) HostJNIToolPath(ctx PathContext, lib string) Path {
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	path := pathForInstall(ctx, ctx.Config().BuildOS, ctx.Config().BuildArch, "lib64", false, lib+ext)
	return path
}

func (c *config) HostJavaToolPath(ctx PathContext, tool string) Path {
	path := pathForInstall(ctx, ctx.Config().BuildOS, ctx.Config().BuildArch, "framework", false, tool)
	return path
}

// PrebuiltOS returns the name of the host OS used in prebuilts directories.
func (c *config) PrebuiltOS() string {
	switch runtime.GOOS {
	case "linux":
		return "linux-x86"
	case "darwin":
		return "darwin-x86"
	default:
		panic("Unknown GOOS")
	}
}

// GoRoot returns the path to the root directory of the Go toolchain.
func (c *config) GoRoot() string {
	return fmt.Sprintf("prebuilts/go/%s", c.PrebuiltOS())
}

// PrebuiltBuildTool returns the path to a tool in the prebuilts directory containing
// checked-in tools, like Kati, Ninja or Toybox, for the current host OS.
func (c *config) PrebuiltBuildTool(ctx PathContext, tool string) Path {
	return PathForSource(ctx, "prebuilts/build-tools", c.PrebuiltOS(), "bin", tool)
}

// CpPreserveSymlinksFlags returns the host-specific flag for the cp(1) command
// to preserve symlinks.
func (c *config) CpPreserveSymlinksFlags() string {
	switch runtime.GOOS {
	case "darwin":
		return "-R"
	case "linux":
		return "-d"
	default:
		return ""
	}
}

func (c *config) Getenv(key string) string {
	var val string
	var exists bool
	c.envLock.Lock()
	defer c.envLock.Unlock()
	if c.envDeps == nil {
		c.envDeps = make(map[string]string)
	}
	if val, exists = c.envDeps[key]; !exists {
		if c.envFrozen {
			panic("Cannot access new environment variables after envdeps are frozen")
		}
		val, _ = c.env[key]
		c.envDeps[key] = val
	}
	return val
}

func (c *config) GetenvWithDefault(key string, defaultValue string) string {
	ret := c.Getenv(key)
	if ret == "" {
		return defaultValue
	}
	return ret
}

func (c *config) IsEnvTrue(key string) bool {
	value := c.Getenv(key)
	return value == "1" || value == "y" || value == "yes" || value == "on" || value == "true"
}

func (c *config) IsEnvFalse(key string) bool {
	value := c.Getenv(key)
	return value == "0" || value == "n" || value == "no" || value == "off" || value == "false"
}

// EnvDeps returns the environment variables this build depends on. The first
// call to this function blocks future reads from the environment.
func (c *config) EnvDeps() map[string]string {
	c.envLock.Lock()
	defer c.envLock.Unlock()
	c.envFrozen = true
	return c.envDeps
}

func (c *config) KatiEnabled() bool {
	return c.katiEnabled
}

func (c *config) BuildId() string {
	return String(c.productVariables.BuildId)
}

// BuildNumberFile returns the path to a text file containing metadata
// representing the current build's number.
//
// Rules that want to reference the build number should read from this file
// without depending on it. They will run whenever their other dependencies
// require them to run and get the current build number. This ensures they don't
// rebuild on every incremental build when the build number changes.
func (c *config) BuildNumberFile(ctx PathContext) Path {
	return PathForOutput(ctx, String(c.productVariables.BuildNumberFile))
}

// DeviceName returns the name of the current device target.
// TODO: take an AndroidModuleContext to select the device name for multi-device builds
func (c *config) DeviceName() string {
	return *c.productVariables.DeviceName
}

// DeviceProduct returns the current product target. There could be multiple of
// these per device type.
//
// NOTE: Do not base conditional logic on this value. It may break product inheritance.
func (c *config) DeviceProduct() string {
	return *c.productVariables.DeviceProduct
}

// HasDeviceProduct returns if the build has a product. A build will not
// necessarily have a product when --skip-config is passed to soong, like it is
// in prebuilts/build-tools/build-prebuilts.sh
func (c *config) HasDeviceProduct() bool {
	return c.productVariables.DeviceProduct != nil
}

func (c *config) DeviceResourceOverlays() []string {
	return c.productVariables.DeviceResourceOverlays
}

func (c *config) ProductResourceOverlays() []string {
	return c.productVariables.ProductResourceOverlays
}

func (c *config) PlatformVersionName() string {
	return String(c.productVariables.Platform_version_name)
}

func (c *config) PlatformSdkVersion() ApiLevel {
	return uncheckedFinalApiLevel(*c.productVariables.Platform_sdk_version)
}

func (c *config) RawPlatformSdkVersion() *int {
	return c.productVariables.Platform_sdk_version
}

func (c *config) PlatformSdkFinal() bool {
	return Bool(c.productVariables.Platform_sdk_final)
}

func (c *config) PlatformSdkCodename() string {
	return String(c.productVariables.Platform_sdk_codename)
}

func (c *config) PlatformSdkExtensionVersion() int {
	return *c.productVariables.Platform_sdk_extension_version
}

func (c *config) PlatformBaseSdkExtensionVersion() int {
	return *c.productVariables.Platform_base_sdk_extension_version
}

func (c *config) PlatformSecurityPatch() string {
	return String(c.productVariables.Platform_security_patch)
}

func (c *config) PlatformPreviewSdkVersion() string {
	return String(c.productVariables.Platform_preview_sdk_version)
}

func (c *config) PlatformMinSupportedTargetSdkVersion() string {
	return String(c.productVariables.Platform_min_supported_target_sdk_version)
}

func (c *config) PlatformBaseOS() string {
	return String(c.productVariables.Platform_base_os)
}

func (c *config) PlatformVersionLastStable() string {
	return String(c.productVariables.Platform_version_last_stable)
}

func (c *config) PlatformVersionKnownCodenames() string {
	return String(c.productVariables.Platform_version_known_codenames)
}

func (c *config) MinSupportedSdkVersion() ApiLevel {
	return uncheckedFinalApiLevel(21)
}

func (c *config) FinalApiLevels() []ApiLevel {
	var levels []ApiLevel
	for i := 1; i <= c.PlatformSdkVersion().FinalOrFutureInt(); i++ {
		levels = append(levels, uncheckedFinalApiLevel(i))
	}
	return levels
}

func (c *config) PreviewApiLevels() []ApiLevel {
	var levels []ApiLevel
	for i, codename := range c.PlatformVersionActiveCodenames() {
		levels = append(levels, ApiLevel{
			value:     codename,
			number:    i,
			isPreview: true,
		})
	}
	return levels
}

func (c *config) LatestPreviewApiLevel() ApiLevel {
	level := NoneApiLevel
	for _, l := range c.PreviewApiLevels() {
		if l.GreaterThan(level) {
			level = l
		}
	}
	return level
}

func (c *config) AllSupportedApiLevels() []ApiLevel {
	var levels []ApiLevel
	levels = append(levels, c.FinalApiLevels()...)
	return append(levels, c.PreviewApiLevels()...)
}

// DefaultAppTargetSdk returns the API level that platform apps are targeting.
// This converts a codename to the exact ApiLevel it represents.
func (c *config) DefaultAppTargetSdk(ctx EarlyModuleContext) ApiLevel {
	// This logic is replicated in starlark, if changing logic here update starlark code too
	// https://cs.android.com/android/platform/superproject/+/master:build/bazel/rules/common/api.bzl;l=72;drc=231c7e8c8038fd478a79eb68aa5b9f5c64e0e061
	if Bool(c.productVariables.Platform_sdk_final) {
		return c.PlatformSdkVersion()
	}
	codename := c.PlatformSdkCodename()
	hostOnlyBuild := c.productVariables.DeviceArch == nil
	if codename == "" {
		// There are some host-only builds (those are invoked by build-prebuilts.sh) which
		// don't set platform sdk codename. Platform sdk codename makes sense only when we
		// are building the platform. So we don't enforce the below panic for the host-only
		// builds.
		if hostOnlyBuild {
			return NoneApiLevel
		}
		panic("Platform_sdk_codename must be set")
	}
	if codename == "REL" {
		panic("Platform_sdk_codename should not be REL when Platform_sdk_final is true")
	}
	return ApiLevelOrPanic(ctx, codename)
}

func (c *config) AppsDefaultVersionName() string {
	return String(c.productVariables.AppsDefaultVersionName)
}

// Codenames that are active in the current lunch target.
func (c *config) PlatformVersionActiveCodenames() []string {
	return c.productVariables.Platform_version_active_codenames
}

// All unreleased codenames.
func (c *config) PlatformVersionAllPreviewCodenames() []string {
	return c.productVariables.Platform_version_all_preview_codenames
}

func (c *config) ProductAAPTConfig() []string {
	return c.productVariables.AAPTConfig
}

func (c *config) ProductAAPTPreferredConfig() string {
	return String(c.productVariables.AAPTPreferredConfig)
}

func (c *config) ProductAAPTCharacteristics() string {
	return String(c.productVariables.AAPTCharacteristics)
}

func (c *config) ProductAAPTPrebuiltDPI() []string {
	return c.productVariables.AAPTPrebuiltDPI
}

func (c *config) DefaultAppCertificateDir(ctx PathContext) SourcePath {
	defaultCert := String(c.productVariables.DefaultAppCertificate)
	if defaultCert != "" {
		return PathForSource(ctx, filepath.Dir(defaultCert))
	}
	return PathForSource(ctx, "build/make/target/product/security")
}

func (c *config) DefaultAppCertificate(ctx PathContext) (pem, key SourcePath) {
	defaultCert := String(c.productVariables.DefaultAppCertificate)
	if defaultCert != "" {
		return PathForSource(ctx, defaultCert+".x509.pem"), PathForSource(ctx, defaultCert+".pk8")
	}
	defaultDir := c.DefaultAppCertificateDir(ctx)
	return defaultDir.Join(ctx, "testkey.x509.pem"), defaultDir.Join(ctx, "testkey.pk8")
}

func (c *config) ApexKeyDir(ctx ModuleContext) SourcePath {
	// TODO(b/121224311): define another variable such as TARGET_APEX_KEY_OVERRIDE
	defaultCert := String(c.productVariables.DefaultAppCertificate)
	if defaultCert == "" || filepath.Dir(defaultCert) == "build/make/target/product/security" {
		// When defaultCert is unset or is set to the testkeys path, use the APEX keys
		// that is under the module dir
		return pathForModuleSrc(ctx)
	}
	// If not, APEX keys are under the specified directory
	return PathForSource(ctx, filepath.Dir(defaultCert))
}

// Certificate for the NetworkStack sepolicy context
func (c *config) MainlineSepolicyDevCertificatesDir(ctx ModuleContext) SourcePath {
	cert := String(c.productVariables.MainlineSepolicyDevCertificates)
	if cert != "" {
		return PathForSource(ctx, cert)
	}
	return c.DefaultAppCertificateDir(ctx)
}

// AllowMissingDependencies configures Blueprint/Soong to not fail when modules
// are configured to depend on non-existent modules. Note that this does not
// affect missing input dependencies at the Ninja level.
func (c *config) AllowMissingDependencies() bool {
	return Bool(c.productVariables.Allow_missing_dependencies)
}

// Returns true if a full platform source tree cannot be assumed.
func (c *config) UnbundledBuild() bool {
	return Bool(c.productVariables.Unbundled_build)
}

// Returns true if building apps that aren't bundled with the platform.
// UnbundledBuild() is always true when this is true.
func (c *config) UnbundledBuildApps() bool {
	return len(c.productVariables.Unbundled_build_apps) > 0
}

// Returns true if building image that aren't bundled with the platform.
// UnbundledBuild() is always true when this is true.
func (c *config) UnbundledBuildImage() bool {
	return Bool(c.productVariables.Unbundled_build_image)
}

// Returns true if building modules against prebuilt SDKs.
func (c *config) AlwaysUsePrebuiltSdks() bool {
	return Bool(c.productVariables.Always_use_prebuilt_sdks)
}

func (c *config) MinimizeJavaDebugInfo() bool {
	return Bool(c.productVariables.MinimizeJavaDebugInfo) && !Bool(c.productVariables.Eng)
}

func (c *config) Debuggable() bool {
	return Bool(c.productVariables.Debuggable)
}

func (c *config) Eng() bool {
	return Bool(c.productVariables.Eng)
}

// DevicePrimaryArchType returns the ArchType for the first configured device architecture, or
// Common if there are no device architectures.
func (c *config) DevicePrimaryArchType() ArchType {
	if androidTargets := c.Targets[Android]; len(androidTargets) > 0 {
		return androidTargets[0].Arch.ArchType
	}
	return Common
}

func (c *config) SanitizeHost() []string {
	return append([]string(nil), c.productVariables.SanitizeHost...)
}

func (c *config) SanitizeDevice() []string {
	return append([]string(nil), c.productVariables.SanitizeDevice...)
}

func (c *config) SanitizeDeviceDiag() []string {
	return append([]string(nil), c.productVariables.SanitizeDeviceDiag...)
}

func (c *config) SanitizeDeviceArch() []string {
	return append([]string(nil), c.productVariables.SanitizeDeviceArch...)
}

func (c *config) EnableCFI() bool {
	if c.productVariables.EnableCFI == nil {
		return true
	}
	return *c.productVariables.EnableCFI
}

func (c *config) DisableScudo() bool {
	return Bool(c.productVariables.DisableScudo)
}

func (c *config) Android64() bool {
	for _, t := range c.Targets[Android] {
		if t.Arch.ArchType.Multilib == "lib64" {
			return true
		}
	}

	return false
}

func (c *config) UseGoma() bool {
	return Bool(c.productVariables.UseGoma)
}

func (c *config) UseRBE() bool {
	return Bool(c.productVariables.UseRBE)
}

func (c *config) UseRBEJAVAC() bool {
	return Bool(c.productVariables.UseRBEJAVAC)
}

func (c *config) UseRBER8() bool {
	return Bool(c.productVariables.UseRBER8)
}

func (c *config) UseRBED8() bool {
	return Bool(c.productVariables.UseRBED8)
}

func (c *config) UseRemoteBuild() bool {
	return c.UseGoma() || c.UseRBE()
}

func (c *config) RunErrorProne() bool {
	return c.IsEnvTrue("RUN_ERROR_PRONE")
}

// XrefCorpusName returns the Kythe cross-reference corpus name.
func (c *config) XrefCorpusName() string {
	return c.Getenv("XREF_CORPUS")
}

// XrefCuEncoding returns the compilation unit encoding to use for Kythe code
// xrefs. Can be 'json' (default), 'proto' or 'all'.
func (c *config) XrefCuEncoding() string {
	if enc := c.Getenv("KYTHE_KZIP_ENCODING"); enc != "" {
		return enc
	}
	return "json"
}

// XrefCuJavaSourceMax returns the maximum number of the Java source files
// in a single compilation unit
const xrefJavaSourceFileMaxDefault = "1000"

func (c Config) XrefCuJavaSourceMax() string {
	v := c.Getenv("KYTHE_JAVA_SOURCE_BATCH_SIZE")
	if v == "" {
		return xrefJavaSourceFileMaxDefault
	}
	if _, err := strconv.ParseUint(v, 0, 0); err != nil {
		fmt.Fprintf(os.Stderr,
			"bad KYTHE_JAVA_SOURCE_BATCH_SIZE value: %s, will use %s",
			err, xrefJavaSourceFileMaxDefault)
		return xrefJavaSourceFileMaxDefault
	}
	return v

}

func (c *config) EmitXrefRules() bool {
	return c.XrefCorpusName() != ""
}

func (c *config) ClangTidy() bool {
	return Bool(c.productVariables.ClangTidy)
}

func (c *config) TidyChecks() string {
	if c.productVariables.TidyChecks == nil {
		return ""
	}
	return *c.productVariables.TidyChecks
}

func (c *config) LibartImgHostBaseAddress() string {
	return "0x60000000"
}

func (c *config) LibartImgDeviceBaseAddress() string {
	return "0x70000000"
}

func (c *config) ArtUseReadBarrier() bool {
	return Bool(c.productVariables.ArtUseReadBarrier)
}

// Enforce Runtime Resource Overlays for a module. RROs supersede static RROs,
// but some modules still depend on it.
//
// More info: https://source.android.com/devices/architecture/rros
func (c *config) EnforceRROForModule(name string) bool {
	enforceList := c.productVariables.EnforceRROTargets

	if len(enforceList) > 0 {
		if InList("*", enforceList) {
			return true
		}
		return InList(name, enforceList)
	}
	return false
}
func (c *config) EnforceRROExcludedOverlay(path string) bool {
	excluded := c.productVariables.EnforceRROExcludedOverlays
	if len(excluded) > 0 {
		return HasAnyPrefix(path, excluded)
	}
	return false
}

func (c *config) ExportedNamespaces() []string {
	return append([]string(nil), c.productVariables.NamespacesToExport...)
}

func (c *config) SourceRootDirs() []string {
	return c.productVariables.SourceRootDirs
}

func (c *config) IncludeTags() []string {
	return c.productVariables.IncludeTags
}

func (c *config) HostStaticBinaries() bool {
	return Bool(c.productVariables.HostStaticBinaries)
}

func (c *config) UncompressPrivAppDex() bool {
	return Bool(c.productVariables.UncompressPrivAppDex)
}

func (c *config) ModulesLoadedByPrivilegedModules() []string {
	return c.productVariables.ModulesLoadedByPrivilegedModules
}

// DexpreoptGlobalConfigPath returns the path to the dexpreopt.config file in
// the output directory, if it was created during the product configuration
// phase by Kati.
func (c *config) DexpreoptGlobalConfigPath(ctx PathContext) OptionalPath {
	if c.productVariables.DexpreoptGlobalConfig == nil {
		return OptionalPathForPath(nil)
	}
	return OptionalPathForPath(
		pathForBuildToolDep(ctx, *c.productVariables.DexpreoptGlobalConfig))
}

// DexpreoptGlobalConfig returns the raw byte contents of the dexpreopt global
// configuration. Since the configuration file was created by Kati during
// product configuration (externally of soong_build), it's not tracked, so we
// also manually add a Ninja file dependency on the configuration file to the
// rule that creates the main build.ninja file. This ensures that build.ninja is
// regenerated correctly if dexpreopt.config changes.
func (c *config) DexpreoptGlobalConfig(ctx PathContext) ([]byte, error) {
	path := c.DexpreoptGlobalConfigPath(ctx)
	if !path.Valid() {
		return nil, nil
	}
	ctx.AddNinjaFileDeps(path.String())
	return os.ReadFile(absolutePath(path.String()))
}

func (c *deviceConfig) WithDexpreopt() bool {
	return c.config.productVariables.WithDexpreopt
}

func (c *config) FrameworksBaseDirExists(ctx PathGlobContext) bool {
	return ExistentPathForSource(ctx, "frameworks", "base", "Android.bp").Valid()
}

func (c *config) VndkSnapshotBuildArtifacts() bool {
	return Bool(c.productVariables.VndkSnapshotBuildArtifacts)
}

func (c *config) HasMultilibConflict(arch ArchType) bool {
	return c.multilibConflicts[arch]
}

func (c *config) PrebuiltHiddenApiDir(_ PathContext) string {
	return String(c.productVariables.PrebuiltHiddenApiDir)
}

func (c *config) BazelModulesForceEnabledByFlag() map[string]struct{} {
	return c.bazelForceEnabledModules
}

func (c *deviceConfig) Arches() []Arch {
	var arches []Arch
	for _, target := range c.config.Targets[Android] {
		arches = append(arches, target.Arch)
	}
	return arches
}

func (c *deviceConfig) BinderBitness() string {
	is32BitBinder := c.config.productVariables.Binder32bit
	if is32BitBinder != nil && *is32BitBinder {
		return "32"
	}
	return "64"
}

func (c *deviceConfig) VendorPath() string {
	if c.config.productVariables.VendorPath != nil {
		return *c.config.productVariables.VendorPath
	}
	return "vendor"
}

func (c *deviceConfig) VndkVersion() string {
	return String(c.config.productVariables.DeviceVndkVersion)
}

func (c *deviceConfig) RecoverySnapshotVersion() string {
	return String(c.config.productVariables.RecoverySnapshotVersion)
}

func (c *deviceConfig) CurrentApiLevelForVendorModules() string {
	return StringDefault(c.config.productVariables.DeviceCurrentApiLevelForVendorModules, "current")
}

func (c *deviceConfig) PlatformVndkVersion() string {
	return String(c.config.productVariables.Platform_vndk_version)
}

func (c *deviceConfig) ProductVndkVersion() string {
	return String(c.config.productVariables.ProductVndkVersion)
}

func (c *deviceConfig) ExtraVndkVersions() []string {
	return c.config.productVariables.ExtraVndkVersions
}

func (c *deviceConfig) VndkUseCoreVariant() bool {
	return Bool(c.config.productVariables.VndkUseCoreVariant)
}

func (c *deviceConfig) SystemSdkVersions() []string {
	return c.config.productVariables.DeviceSystemSdkVersions
}

func (c *deviceConfig) PlatformSystemSdkVersions() []string {
	return c.config.productVariables.Platform_systemsdk_versions
}

func (c *deviceConfig) OdmPath() string {
	if c.config.productVariables.OdmPath != nil {
		return *c.config.productVariables.OdmPath
	}
	return "odm"
}

func (c *deviceConfig) ProductPath() string {
	if c.config.productVariables.ProductPath != nil {
		return *c.config.productVariables.ProductPath
	}
	return "product"
}

func (c *deviceConfig) SystemExtPath() string {
	if c.config.productVariables.SystemExtPath != nil {
		return *c.config.productVariables.SystemExtPath
	}
	return "system_ext"
}

func (c *deviceConfig) BtConfigIncludeDir() string {
	return String(c.config.productVariables.BtConfigIncludeDir)
}

func (c *deviceConfig) DeviceKernelHeaderDirs() []string {
	return c.config.productVariables.DeviceKernelHeaders
}

func (c *deviceConfig) TargetSpecificHeaderPath() string {
	return String(c.config.productVariables.TargetSpecificHeaderPath)
}

// JavaCoverageEnabledForPath returns whether Java code coverage is enabled for
// path. Coverage is enabled by default when the product variable
// JavaCoveragePaths is empty. If JavaCoveragePaths is not empty, coverage is
// enabled for any path which is part of this variable (and not part of the
// JavaCoverageExcludePaths product variable). Value "*" in JavaCoveragePaths
// represents any path.
func (c *deviceConfig) JavaCoverageEnabledForPath(path string) bool {
	coverage := false
	if len(c.config.productVariables.JavaCoveragePaths) == 0 ||
		InList("*", c.config.productVariables.JavaCoveragePaths) ||
		HasAnyPrefix(path, c.config.productVariables.JavaCoveragePaths) {
		coverage = true
	}
	if coverage && len(c.config.productVariables.JavaCoverageExcludePaths) > 0 {
		if HasAnyPrefix(path, c.config.productVariables.JavaCoverageExcludePaths) {
			coverage = false
		}
	}
	return coverage
}

// Returns true if gcov or clang coverage is enabled.
func (c *deviceConfig) NativeCoverageEnabled() bool {
	return Bool(c.config.productVariables.GcovCoverage) ||
		Bool(c.config.productVariables.ClangCoverage)
}

func (c *deviceConfig) ClangCoverageEnabled() bool {
	return Bool(c.config.productVariables.ClangCoverage)
}

func (c *deviceConfig) ClangCoverageContinuousMode() bool {
	return Bool(c.config.productVariables.ClangCoverageContinuousMode)
}

func (c *deviceConfig) GcovCoverageEnabled() bool {
	return Bool(c.config.productVariables.GcovCoverage)
}

// NativeCoverageEnabledForPath returns whether (GCOV- or Clang-based) native
// code coverage is enabled for path. By default, coverage is not enabled for a
// given path unless it is part of the NativeCoveragePaths product variable (and
// not part of the NativeCoverageExcludePaths product variable). Value "*" in
// NativeCoveragePaths represents any path.
func (c *deviceConfig) NativeCoverageEnabledForPath(path string) bool {
	coverage := false
	if len(c.config.productVariables.NativeCoveragePaths) > 0 {
		if InList("*", c.config.productVariables.NativeCoveragePaths) || HasAnyPrefix(path, c.config.productVariables.NativeCoveragePaths) {
			coverage = true
		}
	}
	if coverage && len(c.config.productVariables.NativeCoverageExcludePaths) > 0 {
		// Workaround coverage boot failure.
		// http://b/269981180
		if strings.HasPrefix(path, "external/protobuf") {
			coverage = false
		}
		if HasAnyPrefix(path, c.config.productVariables.NativeCoverageExcludePaths) {
			coverage = false
		}
	}
	return coverage
}

func (c *deviceConfig) PgoAdditionalProfileDirs() []string {
	return c.config.productVariables.PgoAdditionalProfileDirs
}

// AfdoProfile returns fully qualified path associated to the given module name
func (c *deviceConfig) AfdoProfile(name string) (*string, error) {
	for _, afdoProfile := range c.config.productVariables.AfdoProfiles {
		split := strings.Split(afdoProfile, ":")
		if len(split) != 3 {
			return nil, fmt.Errorf("AFDO_PROFILES has invalid value: %s. "+
				"The expected format is <module>:<fully-qualified-path-to-fdo_profile>", afdoProfile)
		}
		if split[0] == name {
			return proptools.StringPtr(strings.Join([]string{split[1], split[2]}, ":")), nil
		}
	}
	return nil, nil
}

func (c *deviceConfig) VendorSepolicyDirs() []string {
	return c.config.productVariables.BoardVendorSepolicyDirs
}

func (c *deviceConfig) OdmSepolicyDirs() []string {
	return c.config.productVariables.BoardOdmSepolicyDirs
}

func (c *deviceConfig) SystemExtPublicSepolicyDirs() []string {
	return c.config.productVariables.SystemExtPublicSepolicyDirs
}

func (c *deviceConfig) SystemExtPrivateSepolicyDirs() []string {
	return c.config.productVariables.SystemExtPrivateSepolicyDirs
}

func (c *deviceConfig) SepolicyM4Defs() []string {
	return c.config.productVariables.BoardSepolicyM4Defs
}

func (c *deviceConfig) OverrideManifestPackageNameFor(name string) (manifestName string, overridden bool) {
	return findOverrideValue(c.config.productVariables.ManifestPackageNameOverrides, name,
		"invalid override rule %q in PRODUCT_MANIFEST_PACKAGE_NAME_OVERRIDES should be <module_name>:<manifest_name>")
}

func (c *deviceConfig) OverrideCertificateFor(name string) (certificatePath string, overridden bool) {
	return findOverrideValue(c.config.productVariables.CertificateOverrides, name,
		"invalid override rule %q in PRODUCT_CERTIFICATE_OVERRIDES should be <module_name>:<certificate_module_name>")
}

func (c *deviceConfig) OverridePackageNameFor(name string) string {
	newName, overridden := findOverrideValue(
		c.config.productVariables.PackageNameOverrides,
		name,
		"invalid override rule %q in PRODUCT_PACKAGE_NAME_OVERRIDES should be <module_name>:<package_name>")
	if overridden {
		return newName
	}
	return name
}

func findOverrideValue(overrides []string, name string, errorMsg string) (newValue string, overridden bool) {
	if overrides == nil || len(overrides) == 0 {
		return "", false
	}
	for _, o := range overrides {
		split := strings.Split(o, ":")
		if len(split) != 2 {
			// This shouldn't happen as this is first checked in make, but just in case.
			panic(fmt.Errorf(errorMsg, o))
		}
		if matchPattern(split[0], name) {
			return substPattern(split[0], split[1], name), true
		}
	}
	return "", false
}

func (c *deviceConfig) ApexGlobalMinSdkVersionOverride() string {
	return String(c.config.productVariables.ApexGlobalMinSdkVersionOverride)
}

func (c *config) IntegerOverflowDisabledForPath(path string) bool {
	if len(c.productVariables.IntegerOverflowExcludePaths) == 0 {
		return false
	}
	return HasAnyPrefix(path, c.productVariables.IntegerOverflowExcludePaths)
}

func (c *config) CFIDisabledForPath(path string) bool {
	if len(c.productVariables.CFIExcludePaths) == 0 {
		return false
	}
	return HasAnyPrefix(path, c.productVariables.CFIExcludePaths)
}

func (c *config) CFIEnabledForPath(path string) bool {
	if len(c.productVariables.CFIIncludePaths) == 0 {
		return false
	}
	return HasAnyPrefix(path, c.productVariables.CFIIncludePaths) && !c.CFIDisabledForPath(path)
}

func (c *config) MemtagHeapDisabledForPath(path string) bool {
	if len(c.productVariables.MemtagHeapExcludePaths) == 0 {
		return false
	}
	return HasAnyPrefix(path, c.productVariables.MemtagHeapExcludePaths)
}

func (c *config) MemtagHeapAsyncEnabledForPath(path string) bool {
	if len(c.productVariables.MemtagHeapAsyncIncludePaths) == 0 {
		return false
	}
	return HasAnyPrefix(path, c.productVariables.MemtagHeapAsyncIncludePaths) && !c.MemtagHeapDisabledForPath(path)
}

func (c *config) MemtagHeapSyncEnabledForPath(path string) bool {
	if len(c.productVariables.MemtagHeapSyncIncludePaths) == 0 {
		return false
	}
	return HasAnyPrefix(path, c.productVariables.MemtagHeapSyncIncludePaths) && !c.MemtagHeapDisabledForPath(path)
}

func (c *config) HWASanEnabledForPath(path string) bool {
	if len(c.productVariables.HWASanIncludePaths) == 0 {
		return false
	}
	return HasAnyPrefix(path, c.productVariables.HWASanIncludePaths)
}

func (c *config) VendorConfig(name string) VendorConfig {
	return soongconfig.Config(c.productVariables.VendorVars[name])
}

func (c *config) NdkAbis() bool {
	return Bool(c.productVariables.Ndk_abis)
}

func (c *config) AmlAbis() bool {
	return Bool(c.productVariables.Aml_abis)
}

func (c *config) FlattenApex() bool {
	return Bool(c.productVariables.Flatten_apex)
}

func (c *config) ForceApexSymlinkOptimization() bool {
	return Bool(c.productVariables.ForceApexSymlinkOptimization)
}

func (c *config) ApexCompressionEnabled() bool {
	return Bool(c.productVariables.CompressedApex) && !c.UnbundledBuildApps()
}

func (c *config) ApexTrimEnabled() bool {
	return Bool(c.productVariables.TrimmedApex)
}

func (c *config) EnforceSystemCertificate() bool {
	return Bool(c.productVariables.EnforceSystemCertificate)
}

func (c *config) EnforceSystemCertificateAllowList() []string {
	return c.productVariables.EnforceSystemCertificateAllowList
}

func (c *config) EnforceProductPartitionInterface() bool {
	return Bool(c.productVariables.EnforceProductPartitionInterface)
}

func (c *config) EnforceInterPartitionJavaSdkLibrary() bool {
	return Bool(c.productVariables.EnforceInterPartitionJavaSdkLibrary)
}

func (c *config) InterPartitionJavaLibraryAllowList() []string {
	return c.productVariables.InterPartitionJavaLibraryAllowList
}

func (c *config) InstallExtraFlattenedApexes() bool {
	return Bool(c.productVariables.InstallExtraFlattenedApexes)
}

func (c *config) ProductHiddenAPIStubs() []string {
	return c.productVariables.ProductHiddenAPIStubs
}

func (c *config) ProductHiddenAPIStubsSystem() []string {
	return c.productVariables.ProductHiddenAPIStubsSystem
}

func (c *config) ProductHiddenAPIStubsTest() []string {
	return c.productVariables.ProductHiddenAPIStubsTest
}

func (c *deviceConfig) TargetFSConfigGen() []string {
	return c.config.productVariables.TargetFSConfigGen
}

func (c *config) ProductPublicSepolicyDirs() []string {
	return c.productVariables.ProductPublicSepolicyDirs
}

func (c *config) ProductPrivateSepolicyDirs() []string {
	return c.productVariables.ProductPrivateSepolicyDirs
}

func (c *config) MissingUsesLibraries() []string {
	return c.productVariables.MissingUsesLibraries
}

func (c *config) TargetMultitreeUpdateMeta() bool {
	return c.productVariables.MultitreeUpdateMeta
}

func (c *deviceConfig) DeviceArch() string {
	return String(c.config.productVariables.DeviceArch)
}

func (c *deviceConfig) DeviceArchVariant() string {
	return String(c.config.productVariables.DeviceArchVariant)
}

func (c *deviceConfig) DeviceSecondaryArch() string {
	return String(c.config.productVariables.DeviceSecondaryArch)
}

func (c *deviceConfig) DeviceSecondaryArchVariant() string {
	return String(c.config.productVariables.DeviceSecondaryArchVariant)
}

func (c *deviceConfig) BoardUsesRecoveryAsBoot() bool {
	return Bool(c.config.productVariables.BoardUsesRecoveryAsBoot)
}

func (c *deviceConfig) BoardKernelBinaries() []string {
	return c.config.productVariables.BoardKernelBinaries
}

func (c *deviceConfig) BoardKernelModuleInterfaceVersions() []string {
	return c.config.productVariables.BoardKernelModuleInterfaceVersions
}

func (c *deviceConfig) BoardMoveRecoveryResourcesToVendorBoot() bool {
	return Bool(c.config.productVariables.BoardMoveRecoveryResourcesToVendorBoot)
}

func (c *deviceConfig) PlatformSepolicyVersion() string {
	return String(c.config.productVariables.PlatformSepolicyVersion)
}

func (c *deviceConfig) TotSepolicyVersion() string {
	return String(c.config.productVariables.TotSepolicyVersion)
}

func (c *deviceConfig) PlatformSepolicyCompatVersions() []string {
	return c.config.productVariables.PlatformSepolicyCompatVersions
}

func (c *deviceConfig) BoardSepolicyVers() string {
	if ver := String(c.config.productVariables.BoardSepolicyVers); ver != "" {
		return ver
	}
	return c.PlatformSepolicyVersion()
}

func (c *deviceConfig) BoardPlatVendorPolicy() []string {
	return c.config.productVariables.BoardPlatVendorPolicy
}

func (c *deviceConfig) BoardReqdMaskPolicy() []string {
	return c.config.productVariables.BoardReqdMaskPolicy
}

func (c *deviceConfig) BoardSystemExtPublicPrebuiltDirs() []string {
	return c.config.productVariables.BoardSystemExtPublicPrebuiltDirs
}

func (c *deviceConfig) BoardSystemExtPrivatePrebuiltDirs() []string {
	return c.config.productVariables.BoardSystemExtPrivatePrebuiltDirs
}

func (c *deviceConfig) BoardProductPublicPrebuiltDirs() []string {
	return c.config.productVariables.BoardProductPublicPrebuiltDirs
}

func (c *deviceConfig) BoardProductPrivatePrebuiltDirs() []string {
	return c.config.productVariables.BoardProductPrivatePrebuiltDirs
}

func (c *deviceConfig) SystemExtSepolicyPrebuiltApiDir() string {
	return String(c.config.productVariables.SystemExtSepolicyPrebuiltApiDir)
}

func (c *deviceConfig) ProductSepolicyPrebuiltApiDir() string {
	return String(c.config.productVariables.ProductSepolicyPrebuiltApiDir)
}

func (c *deviceConfig) IsPartnerTrebleSepolicyTestEnabled() bool {
	return c.SystemExtSepolicyPrebuiltApiDir() != "" || c.ProductSepolicyPrebuiltApiDir() != ""
}

func (c *deviceConfig) DirectedVendorSnapshot() bool {
	return c.config.productVariables.DirectedVendorSnapshot
}

func (c *deviceConfig) VendorSnapshotModules() map[string]bool {
	return c.config.productVariables.VendorSnapshotModules
}

func (c *deviceConfig) DirectedRecoverySnapshot() bool {
	return c.config.productVariables.DirectedRecoverySnapshot
}

func (c *deviceConfig) RecoverySnapshotModules() map[string]bool {
	return c.config.productVariables.RecoverySnapshotModules
}

func createDirsMap(previous map[string]bool, dirs []string) (map[string]bool, error) {
	var ret = make(map[string]bool)
	for _, dir := range dirs {
		clean := filepath.Clean(dir)
		if previous[clean] || ret[clean] {
			return nil, fmt.Errorf("Duplicate entry %s", dir)
		}
		ret[clean] = true
	}
	return ret, nil
}

func (c *deviceConfig) createDirsMapOnce(onceKey OnceKey, previous map[string]bool, dirs []string) map[string]bool {
	dirMap := c.Once(onceKey, func() interface{} {
		ret, err := createDirsMap(previous, dirs)
		if err != nil {
			panic(fmt.Errorf("%s: %w", onceKey.key, err))
		}
		return ret
	})
	if dirMap == nil {
		return nil
	}
	return dirMap.(map[string]bool)
}

var vendorSnapshotDirsExcludedKey = NewOnceKey("VendorSnapshotDirsExcludedMap")

func (c *deviceConfig) VendorSnapshotDirsExcludedMap() map[string]bool {
	return c.createDirsMapOnce(vendorSnapshotDirsExcludedKey, nil,
		c.config.productVariables.VendorSnapshotDirsExcluded)
}

var vendorSnapshotDirsIncludedKey = NewOnceKey("VendorSnapshotDirsIncludedMap")

func (c *deviceConfig) VendorSnapshotDirsIncludedMap() map[string]bool {
	excludedMap := c.VendorSnapshotDirsExcludedMap()
	return c.createDirsMapOnce(vendorSnapshotDirsIncludedKey, excludedMap,
		c.config.productVariables.VendorSnapshotDirsIncluded)
}

var recoverySnapshotDirsExcludedKey = NewOnceKey("RecoverySnapshotDirsExcludedMap")

func (c *deviceConfig) RecoverySnapshotDirsExcludedMap() map[string]bool {
	return c.createDirsMapOnce(recoverySnapshotDirsExcludedKey, nil,
		c.config.productVariables.RecoverySnapshotDirsExcluded)
}

var recoverySnapshotDirsIncludedKey = NewOnceKey("RecoverySnapshotDirsIncludedMap")

func (c *deviceConfig) RecoverySnapshotDirsIncludedMap() map[string]bool {
	excludedMap := c.RecoverySnapshotDirsExcludedMap()
	return c.createDirsMapOnce(recoverySnapshotDirsIncludedKey, excludedMap,
		c.config.productVariables.RecoverySnapshotDirsIncluded)
}

func (c *deviceConfig) HostFakeSnapshotEnabled() bool {
	return c.config.productVariables.HostFakeSnapshotEnabled
}

func (c *deviceConfig) ShippingApiLevel() ApiLevel {
	if c.config.productVariables.ShippingApiLevel == nil {
		return NoneApiLevel
	}
	apiLevel, _ := strconv.Atoi(*c.config.productVariables.ShippingApiLevel)
	return uncheckedFinalApiLevel(apiLevel)
}

func (c *deviceConfig) BuildBrokenClangAsFlags() bool {
	return c.config.productVariables.BuildBrokenClangAsFlags
}

func (c *deviceConfig) BuildBrokenClangCFlags() bool {
	return c.config.productVariables.BuildBrokenClangCFlags
}

func (c *deviceConfig) BuildBrokenClangProperty() bool {
	return c.config.productVariables.BuildBrokenClangProperty
}

func (c *deviceConfig) BuildBrokenEnforceSyspropOwner() bool {
	return c.config.productVariables.BuildBrokenEnforceSyspropOwner
}

func (c *deviceConfig) BuildBrokenTrebleSyspropNeverallow() bool {
	return c.config.productVariables.BuildBrokenTrebleSyspropNeverallow
}

func (c *deviceConfig) BuildBrokenUsesSoongPython2Modules() bool {
	return c.config.productVariables.BuildBrokenUsesSoongPython2Modules
}

func (c *deviceConfig) BuildDebugfsRestrictionsEnabled() bool {
	return c.config.productVariables.BuildDebugfsRestrictionsEnabled
}

func (c *deviceConfig) BuildBrokenVendorPropertyNamespace() bool {
	return c.config.productVariables.BuildBrokenVendorPropertyNamespace
}

func (c *deviceConfig) BuildBrokenInputDir(name string) bool {
	return InList(name, c.config.productVariables.BuildBrokenInputDirModules)
}

func (c *deviceConfig) BuildBrokenDepfile() bool {
	return Bool(c.config.productVariables.BuildBrokenDepfile)
}

func (c *deviceConfig) RequiresInsecureExecmemForSwiftshader() bool {
	return c.config.productVariables.RequiresInsecureExecmemForSwiftshader
}

func (c *config) SelinuxIgnoreNeverallows() bool {
	return c.productVariables.SelinuxIgnoreNeverallows
}

func (c *deviceConfig) SepolicySplit() bool {
	return c.config.productVariables.SepolicySplit
}

func (c *deviceConfig) SepolicyFreezeTestExtraDirs() []string {
	return c.config.productVariables.SepolicyFreezeTestExtraDirs
}

func (c *deviceConfig) SepolicyFreezeTestExtraPrebuiltDirs() []string {
	return c.config.productVariables.SepolicyFreezeTestExtraPrebuiltDirs
}

func (c *deviceConfig) GenerateAidlNdkPlatformBackend() bool {
	return c.config.productVariables.GenerateAidlNdkPlatformBackend
}

func (c *config) IgnorePrefer32OnDevice() bool {
	return c.productVariables.IgnorePrefer32OnDevice
}

func (c *config) BootJars() []string {
	return c.Once(earlyBootJarsKey, func() interface{} {
		list := c.productVariables.BootJars.CopyOfJars()
		return append(list, c.productVariables.ApexBootJars.CopyOfJars()...)
	}).([]string)
}

func (c *config) NonApexBootJars() ConfiguredJarList {
	return c.productVariables.BootJars
}

func (c *config) ApexBootJars() ConfiguredJarList {
	return c.productVariables.ApexBootJars
}

func (c *config) RBEWrapper() string {
	return c.GetenvWithDefault("RBE_WRAPPER", remoteexec.DefaultWrapperPath)
}

// UseHostMusl returns true if the host target has been configured to build against musl libc.
func (c *config) UseHostMusl() bool {
	return Bool(c.productVariables.HostMusl)
}

func (c *config) LogMixedBuild(ctx BaseModuleContext, useBazel bool) {
	moduleName := ctx.Module().Name()
	c.mixedBuildsLock.Lock()
	defer c.mixedBuildsLock.Unlock()
	if useBazel {
		c.mixedBuildEnabledModules[moduleName] = struct{}{}
	} else {
		c.mixedBuildDisabledModules[moduleName] = struct{}{}
	}
}

// ApiSurfaces directory returns the source path inside the api_surfaces repo
// (relative to workspace root).
func (c *config) ApiSurfacesDir(s ApiSurface, version string) string {
	return filepath.Join(
		"build",
		"bazel",
		"api_surfaces",
		s.String(),
		version)
}

func (c *config) BuildFromTextStub() bool {
	return c.buildFromTextStub
}

func (c *config) SetBuildFromTextStub(b bool) {
	c.buildFromTextStub = b
}
func (c *config) AddForceEnabledModules(forceEnabled []string) {
	for _, forceEnabledModule := range forceEnabled {
		c.bazelForceEnabledModules[forceEnabledModule] = struct{}{}
	}
}
