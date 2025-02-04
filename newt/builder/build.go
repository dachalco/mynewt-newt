/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package builder

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"

	log "github.com/sirupsen/logrus"

	"github.com/dachalco/mynewt-newt/newt/interfaces"
	"github.com/dachalco/mynewt-newt/newt/newtutil"
	"github.com/dachalco/mynewt-newt/newt/pkg"
	"github.com/dachalco/mynewt-newt/newt/repo"
	"github.com/dachalco/mynewt-newt/newt/resolve"
	"github.com/dachalco/mynewt-newt/newt/symbol"
	"github.com/dachalco/mynewt-newt/newt/syscfg"
	"github.com/dachalco/mynewt-newt/newt/target"
	"github.com/dachalco/mynewt-newt/newt/toolchain"
	"github.com/dachalco/mynewt-newt/util"
)

type Builder struct {
	PkgMap map[*resolve.ResolvePackage]*BuildPackage

	apiMap           map[string]*BuildPackage
	appPkg           *BuildPackage
	bspPkg           *BuildPackage
	compilerPkg      *BuildPackage
	targetPkg        *BuildPackage
	testPkg          *BuildPackage
	compilerInfo     *toolchain.CompilerInfo
	targetBuilder    *TargetBuilder
	cfg              syscfg.Cfg
	linkerScripts    []string
	buildName        string
	linkElf          string
	injectedSettings map[string]string
}

func NewBuilder(
	t *TargetBuilder,
	buildName string,
	rpkgs []*resolve.ResolvePackage,
	apiMap map[string]*resolve.ResolvePackage,
	cfg syscfg.Cfg) (*Builder, error) {

	b := &Builder{
		PkgMap: make(map[*resolve.ResolvePackage]*BuildPackage, len(rpkgs)),
		cfg:    cfg,

		buildName:        buildName,
		apiMap:           make(map[string]*BuildPackage, len(apiMap)),
		linkElf:          "",
		targetBuilder:    t,
		injectedSettings: map[string]string{},
	}

	for _, rpkg := range rpkgs {
		if _, err := b.addPackage(rpkg); err != nil {
			return nil, err
		}
	}

	// Create the pseudo build packages.
	if _, err := b.addSysinitBpkg(); err != nil {
		return nil, err
	}
	if _, err := b.addUserPreBuildBpkg(); err != nil {
		return nil, err
	}

	for api, rpkg := range apiMap {
		bpkg := b.PkgMap[rpkg]
		if bpkg == nil {
			return nil, util.FmtNewtError(
				"Unexpected unsatisfied API: %s; required by: %s", api,
				rpkg.Lpkg.Name())
		}

		b.apiMap[api] = bpkg
	}

	return b, nil
}

func (b *Builder) addPackage(rpkg *resolve.ResolvePackage) (
	*BuildPackage, error) {

	// Don't allow nil entries to the map
	if rpkg == nil {
		panic("Cannot add nil package builder map")
	}

	bpkg := b.PkgMap[rpkg]
	if bpkg == nil {
		bpkg = NewBuildPackage(rpkg)

		switch bpkg.rpkg.Lpkg.Type() {
		case pkg.PACKAGE_TYPE_APP:
			if b.appPkg != nil {
				return nil, pkgTypeConflictErr(b.appPkg, bpkg)
			}
			b.appPkg = bpkg

		case pkg.PACKAGE_TYPE_BSP:
			if b.bspPkg != nil {
				return nil, pkgTypeConflictErr(b.bspPkg, bpkg)
			}
			b.bspPkg = bpkg

		case pkg.PACKAGE_TYPE_COMPILER:
			if b.compilerPkg != nil {
				return nil, pkgTypeConflictErr(b.compilerPkg, bpkg)
			}
			b.compilerPkg = bpkg

		case pkg.PACKAGE_TYPE_TARGET:
			if b.targetPkg != nil {
				return nil, pkgTypeConflictErr(b.targetPkg, bpkg)
			}
			b.targetPkg = bpkg
		}

		b.PkgMap[rpkg] = bpkg
	}

	return bpkg, nil
}

func pkgTypeConflictErr(p1 *BuildPackage, p2 *BuildPackage) error {
	return util.FmtNewtError("Two %s packages in build: %s, %s",
		pkg.PackageTypeNames[p1.rpkg.Lpkg.Type()],
		p1.rpkg.Lpkg.Name(),
		p2.rpkg.Lpkg.Name())
}

// Recursively compiles all the .c and .s files in the specified directory.
// Architecture-specific files are also compiled.
func collectCompileEntriesDir(srcDir string, c *toolchain.Compiler,
	arch string, ignDirs []string) ([]toolchain.CompilerJob, error) {

	// Quietly succeed if the source directory doesn't exist.
	if util.NodeNotExist(srcDir) {
		return nil, nil
	}

	util.StatusMessage(util.VERBOSITY_VERBOSE,
		"Compiling src in base directory: %s\n", srcDir)

	// Start from the source directory.
	c.SetSrcDir(srcDir)

	// Ignore architecture-specific source files for now.  Use a temporary
	// string slice here so that the "arch" directory is not ignored in the
	// subsequent architecture-specific compile phase.
	ignDirsArch := append(ignDirs, "arch")

	entries := []toolchain.CompilerJob{}

	subEntries, err := c.RecursiveCollectEntries(toolchain.COMPILER_TYPE_C,
		ignDirsArch)
	if err != nil {
		return nil, err
	}
	entries = append(entries, subEntries...)

	// Compile CPP files
	subEntries, err = c.RecursiveCollectEntries(toolchain.COMPILER_TYPE_CPP,
		ignDirsArch)
	if err != nil {
		return nil, err
	}
	entries = append(entries, subEntries...)

	// Copy in pre-compiled library files
	subEntries, err = c.RecursiveCollectEntries(
		toolchain.COMPILER_TYPE_ARCHIVE, ignDirsArch)
	if err != nil {
		return nil, err
	}
	entries = append(entries, subEntries...)

	archDir := srcDir + "/arch/" + arch + "/"
	if util.NodeExist(archDir) {
		util.StatusMessage(util.VERBOSITY_VERBOSE,
			"Compiling architecture specific src pkgs in directory: %s\n",
			archDir)
		c.SetSrcDir(archDir)

		// Compile C source.
		subEntries, err = c.RecursiveCollectEntries(
			toolchain.COMPILER_TYPE_C, ignDirs)
		if err != nil {
			return nil, err
		}
		entries = append(entries, subEntries...)

		// Compile CPP source
		subEntries, err = c.RecursiveCollectEntries(
			toolchain.COMPILER_TYPE_CPP, ignDirs)
		if err != nil {
			return nil, err
		}
		entries = append(entries, subEntries...)

		// Compile assembly source (only architecture-specific).
		subEntries, err = c.RecursiveCollectEntries(
			toolchain.COMPILER_TYPE_ASM, ignDirs)
		if err != nil {
			return nil, err
		}
		entries = append(entries, subEntries...)

		// Copy in pre-compiled library files
		subEntries, err = c.RecursiveCollectEntries(
			toolchain.COMPILER_TYPE_ARCHIVE, ignDirs)
		if err != nil {
			return nil, err
		}
		entries = append(entries, subEntries...)
	}

	return entries, nil
}

// Determines which build profile to use when building the specified package.
// 1. If the package specifies a "pkg.build_profile" value, that is returned:
//
//      pkg.build_profile: debug
//
// 2. Else if the target specifies this package in its
//    "target.package_profiles" map, that value is returned:
//
//      target.package_profiles:
//          'apps/blinky': debug
//          '@apache-mynewt-core/sys/log/full': debug
//
// 3. Else, "" is returned (falls back to the target's general build profile).
func (b *Builder) buildProfileFor(bpkg *BuildPackage) string {
	tgt := b.targetBuilder.GetTarget()
	bp := tgt.PkgProfiles[bpkg.rpkg.Lpkg.FullName()]
	if bp != "" {
		return bp
	}

	return bpkg.BuildProfile(b)
}

func (b *Builder) newCompiler(bpkg *BuildPackage,
	dstDir string) (*toolchain.Compiler, error) {

	var buildProfile string
	if bpkg != nil {
		buildProfile = b.buildProfileFor(bpkg)
	}

	c, err := b.targetBuilder.NewCompiler(dstDir, buildProfile)
	if err != nil {
		// If default build profile was used, just return an error.
		// Otherwise we emit a warning and try with default build profile.
		if buildProfile == "" {
			return nil, err
		}

		log.Warnf("Unsupported build profile for package, using default build profile "+
			"(pkg=\"%s\" build_profile=\"%s\" OS=\"%s\")",
			bpkg.rpkg.Lpkg.FullName(), buildProfile, runtime.GOOS)

		c, err = b.targetBuilder.NewCompiler(dstDir, "")
		if err != nil {
			return nil, err
		}
	}

	c.AddInfo(b.compilerInfo)

	if bpkg != nil {
		log.Debugf("Generating build flags for package %s",
			bpkg.rpkg.Lpkg.FullName())
		ci, err := bpkg.CompilerInfo(b)
		if err != nil {
			return nil, err
		}
		c.AddInfo(ci)
	}

	return c, nil
}

func (b *Builder) collectCompileEntriesBpkg(bpkg *BuildPackage) (
	[]toolchain.CompilerJob, error) {

	c, err := b.newCompiler(bpkg, b.PkgBinDir(bpkg))
	if err != nil {
		return nil, err
	}

	srcDirs := []string{}

	if len(bpkg.SourceDirectories) > 0 {
		for _, relDir := range bpkg.SourceDirectories {
			dir := bpkg.rpkg.Lpkg.BasePath() + "/" + relDir
			if util.NodeNotExist(dir) {
				return nil, util.NewNewtError(fmt.Sprintf(
					"Specified source directory %s, does not exist.",
					dir))
			}
			srcDirs = append(srcDirs, dir)
		}
	} else {
		srcDir := bpkg.rpkg.Lpkg.BasePath() + "/src"
		if util.NodeNotExist(srcDir) {
			// Nothing to compile.
			return nil, nil
		}

		srcDirs = append(srcDirs, srcDir)
	}

	entries := []toolchain.CompilerJob{}
	for _, dir := range srcDirs {
		subEntries, err := collectCompileEntriesDir(dir, c,
			b.targetBuilder.bspPkg.Arch, nil)
		if err != nil {
			return nil, err
		}

		entries = append(entries, subEntries...)
	}

	return entries, nil
}

func (b *Builder) CollectCompileEntriesBpkg(bpkg *BuildPackage) (
	[]toolchain.CompilerJob, error) {
	return b.collectCompileEntriesBpkg(bpkg)
}

func (b *Builder) createArchive(c *toolchain.Compiler,
	bpkg *BuildPackage) error {

	// Create a static library ("archive").
	c.SetSrcDir(bpkg.rpkg.Lpkg.RelativePath())
	archiveFile := b.ArchivePath(bpkg)
	if err := c.CompileArchive(archiveFile); err != nil {
		return err
	}

	return nil
}

func (b *Builder) RemovePackages(cmn map[string]bool) error {
	for pkgName, _ := range cmn {
		for lp, bpkg := range b.PkgMap {
			if bpkg.rpkg.Lpkg.Name() == pkgName {
				delete(b.PkgMap, lp)
			}
		}
	}
	return nil
}

func (b *Builder) ExtractSymbolInfo() (error, *symbol.SymbolMap) {
	syms := symbol.NewSymbolMap()
	for _, bpkg := range b.PkgMap {
		err, sm := b.ParseObjectLibrary(bpkg)
		if err == nil {
			syms, err = (*syms).Merge(sm)
			if err != nil {
				return err, nil
			}
		}
	}
	return nil, syms
}

func (b *Builder) link(elfName string, linkerScripts []string,
	keepSymbols []string, extraADirs []string) error {

	c, err := b.newCompiler(b.appPkg, b.FileBinDir(elfName))
	if err != nil {
		return err
	}

	// Calculate the list of directories containing source .a files.
	var dirs []string
	for _, bpkg := range b.sortedBuildPackages() {
		dirs = append(dirs, b.PkgBinDir(bpkg))

		// Collect lflags from all constituent packages.  Discard everything
		// from the compiler info except lflags; that is all that is relevant
		// to the link command.
		ci, err := bpkg.CompilerInfo(b)
		if err != nil {
			return err
		}
		c.AddInfo(&toolchain.CompilerInfo{Lflags: ci.Lflags})
	}
	dirs = append(dirs, extraADirs...)

	// Find all .a files in the input directories.
	trimmedANames := []string{}
	for _, dir := range dirs {
		fullANames, _ := filepath.Glob(dir + "/*.a")
		for i, archiveName := range fullANames {
			fullANames[i] = filepath.ToSlash(archiveName)
		}
		trimmedANames = append(trimmedANames, fullANames...)
	}

	c.LinkerScripts = linkerScripts
	err = c.CompileElf(elfName, trimmedANames, keepSymbols, b.linkElf)
	if err != nil {
		return err
	}

	return nil
}

// Populates the builder with all the packages that need to be built and
// configures each package's build settings.  After this function executes,
// packages are ready to be built.
func (b *Builder) PrepBuild() error {
	// Populate the base set of compiler flags.  Flags from the following
	// packages get applied to every source file:
	//     * target
	//     * app (if present)
	//     * bsp
	//     * compiler (not added here)
	//
	// In the case of conflicting flags, the higher priority package's flag
	// wins.  Package priorities are assigned as follows (highest priority
	// first):
	//     * target
	//     * app (if present)
	//     * bsp
	//     * <library package>
	//     * compiler
	baseCi := toolchain.NewCompilerInfo()

	// Target flags.
	log.Debugf("Generating build flags for target %s",
		b.targetPkg.rpkg.Lpkg.FullName())
	targetCi, err := b.targetPkg.CompilerInfo(b)
	if err != nil {
		return err
	}
	baseCi.AddCompilerInfo(targetCi)

	// App flags.
	if b.appPkg != nil {
		log.Debugf("Generating build flags for app %s",
			b.appPkg.rpkg.Lpkg.FullName())
		appCi, err := b.appPkg.CompilerInfo(b)
		if err != nil {
			return err
		}

		baseCi.AddCompilerInfo(appCi)
	}

	// Bsp flags.
	log.Debugf("Generating build flags for bsp %s",
		b.bspPkg.rpkg.Lpkg.FullName())
	bspCi, err := b.bspPkg.CompilerInfo(b)
	if err != nil {
		return err
	}

	// Define a cpp symbol indicating the BSP architecture, name of the
	// BSP and app.
	// The arch, app, and bsp defines are kept here for backwards compatiblity.
	// Users should prefer the equivalent syscfg defines.
	archName := b.targetBuilder.bspPkg.Arch
	bspCi.Cflags = append(bspCi.Cflags, "-DARCH_"+util.CIdentifier(archName))
	bspCi.Cflags = append(bspCi.Cflags, "-DARCH_NAME="+archName+"")

	if b.appPkg != nil {
		appName := filepath.Base(b.appPkg.rpkg.Lpkg.Name())
		bspCi.Cflags = append(bspCi.Cflags, "-DAPP_"+util.CIdentifier(appName))
		bspCi.Cflags = append(bspCi.Cflags, "-DAPP_NAME="+appName+"")
	}

	bspName := filepath.Base(b.bspPkg.rpkg.Lpkg.Name())
	bspCi.Cflags = append(bspCi.Cflags, "-DBSP_"+util.CIdentifier(bspName))
	bspCi.Cflags = append(bspCi.Cflags, "-DBSP_NAME="+bspName+"")

	baseCi.AddCompilerInfo(bspCi)

	// All packages have access to the generated code header directory.
	baseCi.Includes = append(baseCi.Includes,
		GeneratedIncludeDir(b.targetPkg.rpkg.Lpkg.FullName()))

	// All packages have access to the user generated header directory.
	baseCi.Includes = append(baseCi.Includes,
		UserPreBuildIncludeDir(b.targetPkg.rpkg.Lpkg.Name()))

	// Let multiplatform libraries know that a Mynewt binary is being build.
	baseCi.Cflags = append(baseCi.Cflags, "-DMYNEWT=1")

	// Note: The compiler package's flags get added at the end, after the flags
	// for library package being built are calculated.
	b.compilerInfo = baseCi

	return nil
}

func (b *Builder) AddCompilerInfo(info *toolchain.CompilerInfo) {
	b.compilerInfo.AddCompilerInfo(info)
}

// addPseudoBpkg creates a dynamic build package and adds it to the builder.
func (b *Builder) addPseudoBpkg(name string,
	dir string) (*BuildPackage, error) {

	lpkg := pkg.NewLocalPackage(b.targetPkg.rpkg.Lpkg.Repo().(*repo.Repo), dir)
	lpkg.SetName(fmt.Sprintf(
		"%s-%s", pkg.ShortName(b.targetPkg.rpkg.Lpkg), name))
	lpkg.SetType(pkg.PACKAGE_TYPE_GENERATED)

	rpkg := resolve.NewResolvePkg(lpkg)
	return b.addPackage(rpkg)
}

// addSysinitBpkg adds the pseudo sysinit build package to the builder.
func (b *Builder) addSysinitBpkg() (*BuildPackage, error) {
	name := fmt.Sprintf("%s-%s", "sysinit", b.buildName)
	dir := GeneratedBaseDir(b.targetPkg.rpkg.Lpkg.FullName())
	return b.addPseudoBpkg(name, dir)
}

// addUserPreBuildBpkg adds the pseudo user build package to the builder.  The
// user build package contains inputs emitted by external scripts.
func (b *Builder) addUserPreBuildBpkg() (*BuildPackage, error) {
	return b.addPseudoBpkg("user-pre-build",
		UserPreBuildDir(b.targetPkg.rpkg.Lpkg.FullName()))
}

// Runs build jobs while any remain.  On failure, signals the other workers to
// stop via the stop channel.  On error, the error object is signaled via the
// results channel.  On successful completion, nil is signaled via the results
// channel.
func buildWorker(
	id int,
	jobs <-chan toolchain.CompilerJob,
	stop chan struct{},
	results chan error) {

	// Execute each job until failure or until a stop is signalled.
	for {
		select {
		case s := <-stop:
			// Re-enqueue the stop signal for the other routines.
			stop <- s

			// Terminate this go routine.
			results <- nil
			return

		case j := <-jobs:
			if err := toolchain.RunJob(j); err != nil {
				// Stop the other routines.
				stop <- struct{}{}

				// Report the error back to the master thread and terminate.
				results <- err
				return
			}

		default:
			// Terminate this go routine.
			results <- nil
			return
		}
	}
}

func (b *Builder) Build() error {
	b.CleanArtifacts()

	// Build the packages alphabetically to ensure a consistent order.
	bpkgs := b.sortedBuildPackages()

	// Calculate the list of jobs.  Each record represents a single file that
	// needs to be compiled.
	entries := []toolchain.CompilerJob{}
	bpkgCompilerMap := map[*BuildPackage]*toolchain.Compiler{}
	for _, bpkg := range bpkgs {
		subEntries, err := b.collectCompileEntriesBpkg(bpkg)
		if err != nil {
			return err
		}
		entries = append(entries, subEntries...)

		if len(subEntries) > 0 {
			bpkgCompilerMap[bpkg] = subEntries[0].Compiler
		}
	}

	// Build each file in parallel.
	jobs := make(chan toolchain.CompilerJob, len(entries))
	defer close(jobs)

	stop := make(chan struct{}, newtutil.NewtNumJobs)
	defer close(stop)

	errors := make(chan error, newtutil.NewtNumJobs)
	defer close(errors)

	for _, entry := range entries {
		jobs <- entry
	}

	for i := 0; i < newtutil.NewtNumJobs; i++ {
		go buildWorker(i, jobs, stop, errors)
	}

	var err error
	for i := 0; i < newtutil.NewtNumJobs; i++ {
		subErr := <-errors
		if err == nil && subErr != nil {
			err = subErr
		}
	}
	if err != nil {
		return err
	}

	for _, bpkg := range bpkgs {
		c := bpkgCompilerMap[bpkg]
		if c != nil {
			if err := b.createArchive(c, bpkg); err != nil {
				return err
			}
		}
	}

	var compileCommands []toolchain.CompileCommand

	for _, bpkg := range bpkgs {
		c := bpkgCompilerMap[bpkg]
		if c != nil {
			compileCommands = append(compileCommands,
				c.GetCompileCommands()...)
		}
	}

	projectPath := interfaces.GetProject().Path() + "/"
	for i := range compileCommands {
		compileCommands[i].Directory = projectPath
	}

	cmdBytes, err := json.MarshalIndent(compileCommands, "", "    ")
	if err != nil {
		log.Error("Unable to encode compilation commands as JSON")
		return nil
	}

	cmdPath := b.CompileCmdsPath()
	errWrite := ioutil.WriteFile(cmdPath, cmdBytes, 0644)
	if errWrite != nil {
		return util.FmtNewtError(
			"Unable to write compile_commands.json file; reason: %s",
			errWrite.Error())
	}

	return nil
}

func (b *Builder) Link(linkerScripts []string, extraADirs []string) error {
	if err := b.link(b.AppElfPath(), linkerScripts, nil,
		extraADirs); err != nil {

		return err
	}
	return nil
}

func (b *Builder) KeepLink(linkerScripts []string, keepMap *symbol.SymbolMap,
	extraADirs []string) error {

	keepSymbols := make([]string, 0)

	if keepMap != nil {
		for _, info := range *keepMap {
			keepSymbols = append(keepSymbols, info.Name)
		}
	}

	if err := b.link(b.AppElfPath(), linkerScripts, keepSymbols,
		extraADirs); err != nil {

		return err
	}

	return nil
}

func (b *Builder) TentativeLink(linkerScripts []string,
	extraADirs []string) error {

	if err := b.link(b.AppTentativeElfPath(), linkerScripts, nil,
		extraADirs); err != nil {

		return err
	}

	return nil
}

func (b *Builder) pkgWithPath(path string) *BuildPackage {
	for _, bpkg := range b.PkgMap {
		if bpkg.rpkg.Lpkg.BasePath() == path {
			return bpkg
		}
	}

	return nil
}

func (b *Builder) FetchSymbolMap() (error, *symbol.SymbolMap) {
	loaderSm := symbol.NewSymbolMap()

	for _, bpkg := range b.PkgMap {
		err, sm := b.ParseObjectLibrary(bpkg)
		if err == nil {
			util.StatusMessage(util.VERBOSITY_VERBOSE,
				"Size of %s Loader Map %d\n", bpkg.rpkg.Lpkg.Name(), len(*sm))
			loaderSm, err = loaderSm.Merge(sm)
			if err != nil {
				return err, nil
			}
		}
	}

	return nil, loaderSm
}

func (b *Builder) GetCompilerInfo() *toolchain.CompilerInfo {
	return b.compilerInfo
}

func (b *Builder) GetTarget() *target.Target {
	return b.targetBuilder.GetTarget()
}

func (b *Builder) buildRomElf(common *symbol.SymbolMap) error {
	// check dependencies on the ROM ELF.  This is really dependent on
	// all of the .a files, but since we already depend on the loader
	// .as to build the initial elf, we only need to check the app .a
	c, err := b.targetBuilder.NewCompiler(b.AppElfPath(), "")
	d := toolchain.NewDepTracker(c)
	if err != nil {
		return err
	}

	archNames := []string{}

	// build the set of archive file names
	for _, bpkg := range b.PkgMap {
		archiveNames, _ := filepath.Glob(b.PkgBinDir(bpkg) + "/*.a")
		archNames = append(archNames, archiveNames...)
	}

	bld, err := d.RomElfBuildRequired(b.AppLinkerElfPath(),
		b.AppElfPath(), archNames)
	if err != nil {
		return err
	}

	if !bld {
		return nil
	}

	util.StatusMessage(util.VERBOSITY_DEFAULT,
		"Generating ROM elf \n")

	/* the linker needs these symbols kept for the split app
	 * to initialize the loader data and bss */
	common.Add(*symbol.NewElfSymbol("__HeapBase"))
	common.Add(*symbol.NewElfSymbol("__bss_start__"))
	common.Add(*symbol.NewElfSymbol("__bss_end__"))
	common.Add(*symbol.NewElfSymbol("__etext"))
	common.Add(*symbol.NewElfSymbol("__data_start__"))
	common.Add(*symbol.NewElfSymbol("__data_end__"))

	/* the split app may need this to access interrupts */
	common.Add(*symbol.NewElfSymbol("__vector_tbl_reloc__"))
	common.Add(*symbol.NewElfSymbol("__isr_vector"))

	err = b.CopySymbols(common)
	if err != nil {
		return err
	}

	/* These symbols are needed by the split app so it can zero
	 * bss and copy data from the loader app before it restarts,
	 * but we have to rename them since it has its own copies of
	 * these special linker symbols  */
	tmp_sm := symbol.NewSymbolMap()
	tmp_sm.Add(*symbol.NewElfSymbol("__HeapBase"))
	tmp_sm.Add(*symbol.NewElfSymbol("__bss_start__"))
	tmp_sm.Add(*symbol.NewElfSymbol("__bss_end__"))
	tmp_sm.Add(*symbol.NewElfSymbol("__etext"))
	tmp_sm.Add(*symbol.NewElfSymbol("__data_start__"))
	tmp_sm.Add(*symbol.NewElfSymbol("__data_end__"))
	err = c.RenameSymbols(tmp_sm, b.AppLinkerElfPath(), "_loader")

	if err != nil {
		return err
	}
	return nil
}

// Deletes files that should never be reused for a subsequent build.  This
// list includes:
//     <app>.img
//     <app>.elf.bin
//     manifest.json
func (b *Builder) CleanArtifacts() {
	if b.appPkg == nil {
		return
	}

	paths := []string{
		b.AppImgPath(),
		b.AppBinPath(),
		b.ManifestPath(),
	}

	// Attempt to delete each artifact, ignoring errors.
	for _, p := range paths {
		os.Remove(p)
	}
}
