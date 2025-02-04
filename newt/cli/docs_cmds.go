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

package cli

import (
	"os"

	"github.com/dachalco/mynewt-newt/newt/docs"
	"github.com/spf13/cobra"
)

func docsBuildRunCmd(cmd *cobra.Command, args []string) {
	wd, _ := os.Getwd()
	db, _ := docs.NewDocsBuilder()
	db.Build(wd + "/_build")
}

func AddDocsCommands(cmd *cobra.Command) {
	docsCmdHelpText := ""
	docsCmdHelpEx := ""
	docsCmd := &cobra.Command{
		Use:     "docs",
		Short:   "Project documentation generation commands",
		Long:    docsCmdHelpText,
		Example: docsCmdHelpEx,
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Usage()
		},
	}

	cmd.AddCommand(docsCmd)

	buildShortHelp := "Generate project documentation using Mynewt docs system."

	buildCmd := &cobra.Command{
		Use:   "build [<outdir>]",
		Short: buildShortHelp,
		Long:  buildShortHelp,
		Run:   docsBuildRunCmd,
	}

	docsCmd.AddCommand(buildCmd)
}
