// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"

	"github.com/MakeNowJust/heredoc/v2"
	"github.com/azure/azure-dev/cli/azd/cmd/actions"
	"github.com/azure/azure-dev/cli/azd/internal"
	"github.com/azure/azure-dev/cli/azd/pkg/account"
	"github.com/azure/azure-dev/cli/azd/pkg/auth"
	"github.com/azure/azure-dev/cli/azd/pkg/commands/pipeline"
	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/environment/azdcontext"
	"github.com/azure/azure-dev/cli/azd/pkg/exec"
	"github.com/azure/azure-dev/cli/azd/pkg/infra/provisioning"
	"github.com/azure/azure-dev/cli/azd/pkg/input"
	"github.com/azure/azure-dev/cli/azd/pkg/output"
	"github.com/azure/azure-dev/cli/azd/pkg/output/ux"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/azcli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type pipelineConfigFlags struct {
	pipeline.PipelineManagerArgs
	global *internal.GlobalCommandOptions
	envFlag
}

func (pc *pipelineConfigFlags) Bind(local *pflag.FlagSet, global *internal.GlobalCommandOptions) {
	local.StringVar(
		&pc.PipelineServicePrincipalName,
		"principal-name",
		"",
		"The name of the service principal to use to grant access to Azure resources as part of the pipeline.",
	)
	local.StringVar(
		&pc.PipelineRemoteName,
		"remote-name",
		"origin",
		"The name of the git remote to configure the pipeline to run on.",
	)
	//nolint:lll
	local.StringVar(
		&pc.PipelineAuthTypeName,
		"auth-type",
		"",
		"The authentication type used between the pipeline provider and Azure for deployment (Only valid for GitHub provider). Valid values: federated, client-credentials.",
	)
	//nolint:lll
	local.StringArrayVar(
		&pc.PipelineRoleNames,
		"principal-role",
		pipeline.DefaultRoleNames,
		"The roles to assign to the service principal. By default the service principal will be granted the Contributor and User Access Administrator roles.",
	)
	// default provider is empty because it can be set from azure.yaml. By letting default here be empty, we know that
	// there no customer input using --provider
	local.StringVar(&pc.PipelineProvider, "provider", "",
		"The pipeline provider to use (github for Github Actions and azdo for Azure Pipelines).")
	pc.envFlag.Bind(local, global)
	pc.global = global
}

func pipelineActions(root *actions.ActionDescriptor) *actions.ActionDescriptor {
	group := root.Add("pipeline", &actions.ActionDescriptorOptions{
		Command: &cobra.Command{
			Use:   "pipeline",
			Short: fmt.Sprintf("Manage and configure your deployment pipelines. %s", output.WithWarningFormat("(Beta)")),
		},
		HelpOptions: actions.ActionHelpOptions{
			Description: getCmdPipelineHelpDescription,
			Footer:      getCmdPipelineHelpFooter,
		},
		GroupingOptions: actions.CommandGroupOptions{
			RootLevelHelp: actions.CmdGroupMonitor,
		},
	})

	group.Add("config", &actions.ActionDescriptorOptions{
		Command:        newPipelineConfigCmd(),
		FlagsResolver:  newPipelineConfigFlags,
		ActionResolver: newPipelineConfigAction,
		HelpOptions: actions.ActionHelpOptions{
			Description: getCmdPipelineConfigHelpDescription,
			Footer:      getCmdPipelineConfigHelpFooter,
		},
	})

	return group
}

func newPipelineConfigFlags(cmd *cobra.Command, global *internal.GlobalCommandOptions) *pipelineConfigFlags {
	flags := &pipelineConfigFlags{}
	flags.Bind(cmd.Flags(), global)

	return flags
}

func newPipelineConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use: "config",
		Short: fmt.Sprintf(
			"Configure your deployment pipeline to connect securely to Azure. %s",
			output.WithWarningFormat("(Beta)")),
	}
}

// pipelineConfigAction defines the action for pipeline config command
type pipelineConfigAction struct {
	flags              *pipelineConfigFlags
	manager            *pipeline.PipelineManager
	accountManager     account.Manager
	azCli              azcli.AzCli
	azdCtx             *azdcontext.AzdContext
	env                *environment.Environment
	console            input.Console
	commandRunner      exec.CommandRunner
	credentialProvider account.SubscriptionCredentialProvider
}

func newPipelineConfigAction(
	azCli azcli.AzCli,
	credentialProvider account.SubscriptionCredentialProvider,
	azdCtx *azdcontext.AzdContext,
	env *environment.Environment,
	accountManager account.Manager,
	_ auth.LoggedInGuard,
	console input.Console,
	flags *pipelineConfigFlags,
	commandRunner exec.CommandRunner,
	subResolver account.SubscriptionTenantResolver,
	userProfile *azcli.UserProfileService,
) actions.Action {
	pca := &pipelineConfigAction{
		flags:              flags,
		azCli:              azCli,
		credentialProvider: credentialProvider,
		manager: pipeline.NewPipelineManager(
			azCli, azdCtx, env, flags.global, commandRunner, console, flags.PipelineManagerArgs, subResolver, userProfile,
		),
		azdCtx:         azdCtx,
		accountManager: accountManager,
		env:            env,
		console:        console,
		commandRunner:  commandRunner,
	}

	return pca
}

// Run implements action interface
func (p *pipelineConfigAction) Run(ctx context.Context) (*actions.ActionResult, error) {
	err := provisioning.EnsureEnv(ctx, p.console, p.env, p.accountManager)
	if err != nil {
		return nil, err
	}

	credential, err := p.credentialProvider.CredentialForSubscription(ctx, p.env.GetSubscriptionId())
	if err != nil {
		return nil, err
	}

	// Detect the SCM and CI providers based on the project directory
	p.manager.ScmProvider,
		p.manager.CiProvider,
		err = pipeline.DetectProviders(
		ctx, p.azdCtx, p.env, p.manager.PipelineProvider, p.console, credential, p.commandRunner,
	)
	if err != nil {
		return nil, err
	}

	pipelineProviderName := p.manager.CiProvider.Name()

	// Command title
	p.console.MessageUxItem(ctx, &ux.MessageTitle{
		Title: fmt.Sprintf("Configure your %s pipeline", pipelineProviderName),
	})

	pipelineResult, err := p.manager.Configure(ctx)
	if err != nil {
		return nil, err
	}

	return &actions.ActionResult{
		Message: &actions.ResultMessage{
			Header: fmt.Sprintf("Your %s pipeline has been configured!", pipelineProviderName),
			FollowUp: heredoc.Docf(`
			Link to view your new repo: %s
			Link to view your pipeline status: %s`,
				output.WithLinkFormat("%s", pipelineResult.RepositoryLink),
				output.WithLinkFormat("%s", pipelineResult.PipelineLink)),
		},
	}, nil
}

func getCmdPipelineHelpDescription(*cobra.Command) string {
	return generateCmdHelpDescription(
		fmt.Sprintf("Manage integrating your application with deployment pipelines. %s", output.WithWarningFormat("(Beta)")),
		[]string{
			formatHelpNote(
				"azd commands (e.g. " +
					output.WithHighLightFormat("provision") + ", " +
					output.WithHighLightFormat("deploy") + ") " +
					"can be used within GitHub Actions and Azure Pipelines to test your code against real Azure resources " +
					"and facilitate deployments."),
			formatHelpNote(
				"After creating a pipeline definition file, running " +
					output.WithHighLightFormat("pipeline config") +
					" will help configure your deployment pipeline to connect securely to Azure."),
			formatHelpNote(fmt.Sprintf("For more information on how to use azd in your pipeline, go to: %s.",
				output.WithLinkFormat("https://aka.ms/azure-dev/pipeline"))),
		})
}

func getCmdPipelineHelpFooter(c *cobra.Command) string {
	return generateCmdHelpSamplesBlock(map[string]string{
		"Walk through the steps required " +
			"to set up your deployment pipeline.": output.WithHighLightFormat("azd pipeline config"),
	})
}

func getCmdPipelineConfigHelpDescription(*cobra.Command) string {
	return generateCmdHelpDescription(
		"Configure your deployment pipeline to connect securely to Azure",
		[]string{
			formatHelpNote(
				"Supports GitHub Actions and Azure Pipelines. To configure using a specific pipeline provider, " +
					"provide a value for the '--provider' flag."),
			formatHelpNote(
				output.WithHighLightFormat("pipeline config") +
					" creates or uses a service principal on the Azure subscription to create a secure connection between" +
					" your deployment pipeline and Azure."),
			formatHelpNote("By default, " +
				output.WithHighLightFormat("pipeline config") +
				" will set deployment pipeline variables and secrets using the current environment. " +
				"To configure for a new or an existing environment, provide a value for the '-e' flag."),
		})
}

func getCmdPipelineConfigHelpFooter(c *cobra.Command) string {
	return generateCmdHelpSamplesBlock(map[string]string{
		"Configure a deployment pipeline using an existing service principal": fmt.Sprintf("%s %s",
			output.WithHighLightFormat("azd pipeline config --principal-name"),
			output.WithWarningFormat("[Principal name]"),
		),
		"Configure a deployment pipeline for 'app-test' environment": fmt.Sprintf("%s %s",
			output.WithHighLightFormat("azd pipeline config -e"),
			output.WithWarningFormat("app-test"),
		),
		"Configure a deployment pipeline for 'app-test' environment on Azure Pipelines.": fmt.Sprintf("%s %s %s",
			output.WithHighLightFormat("azd pipeline config -e"),
			output.WithWarningFormat("app-test"),
			output.WithHighLightFormat("--provider azdo"),
		),
	})
}
