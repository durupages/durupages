// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"encoding/json"
	"net/url"
)

// deploymentHelp documents the `duru deployment` command group.
const deploymentHelp = `usage: duru --page <id> deployment <subcommand> [flags]

subcommands:
  list                     list the page's deployments, newest first
  upload --dir <d>         upload a build output as a new deployment
  activate <deploymentId>  switch the page's active deployment

Every subcommand works on the page named by the shared --page flag (env
DURUPAGES_PAGE):

  duru --page blog deployment upload --dir ./build-output

Run "duru deployment <subcommand> -h" for that subcommand's flags.
`

// cmdDeployment dispatches the `duru deployment` subcommands.
func (c *cli) cmdDeployment(args []string) error {
	sub, rest := splitCommand(args)
	if sub == "" {
		return usagef("missing subcommand\n\n%s", deploymentHelp)
	}
	c.cmdName = "deployment " + sub
	switch sub {
	case "-h", "-help", "--help", "help":
		c.stdoutPrint(deploymentHelp)
		return errHelp
	case "list":
		return c.deploymentList(rest)
	case "upload":
		return c.deploymentUpload(rest)
	case "activate":
		return c.deploymentActivate(rest)
	default:
		c.cmdName = "deployment"
		return usagef("unknown subcommand %q\n\n%s", sub, deploymentHelp)
	}
}

const deploymentListHelp = `usage: duru --page <id> deployment list [flags]

GET /v1/pages/{id}/deployments. Prints {"deployments":[...]} to stdout, newest
first; the active one is marked with "active": true.`

// deploymentList implements `duru deployment list`.
func (c *cli) deploymentList(args []string) error {
	fs := newFlagSet("deployment list")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, deploymentListHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --page <id> deployment list [flags]", &g, resPage); err != nil {
		return err
	}
	pageID, err := g.require(resPage)
	if err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.get(pagePath(pageID) + "/deployments")
	if err != nil {
		return err
	}
	return c.printJSON(body)
}

const deploymentUploadHelp = `usage: duru --page <id> deployment upload --dir ./build-output [flags]

POST /v1/pages/{id}/deployments. The wrangler build output directory is
streamed to the controller as a tar.gz; the controller scans it, uploads it to
object storage and registers the deployment. The page must already exist
("duru page set" creates it). Projects using functions/ must be precompiled
with "wrangler pages functions build" first.

The new deployment is activated unless --no-activate is given, which is how you
stage a build and switch to it later with "duru deployment activate".

The upload summary (deployment id, manifest counts) is printed to stdout.`

// deploymentUpload implements `duru deployment upload`.
func (c *cli) deploymentUpload(args []string) error {
	fs := newFlagSet("deployment upload")
	var g commonFlags
	g.register(fs)
	dir := fs.String("dir", ".", "wrangler build output directory")
	depID := fs.String("deployment", "", "deployment id (default: assigned by the controller)")
	noActivate := fs.Bool("no-activate", false, "register the deployment without making it active")
	pos, err := c.parse(fs, deploymentUploadHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --page <id> deployment upload --dir <dir> [flags]", &g, resPage); err != nil {
		return err
	}
	pageID, err := g.require(resPage)
	if err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.uploadDeployment(*dir, pageID, *depID, !*noActivate)
	if err != nil {
		return err
	}

	var res uploadResult
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	state := "not activated"
	if res.Activated {
		state = "activated"
	}
	c.notef("deployment %s uploaded to page %q (%s)", res.DeploymentID, pageID, state)
	return c.printJSON(body)
}

const deploymentActivateHelp = `usage: duru --page <id> deployment activate <deploymentId> [flags]

POST /v1/pages/{id}/deployments/{deploymentId}/activate. The switch is atomic,
and the deployment must already belong to the page. The updated page is printed
to stdout.`

// deploymentActivate implements `duru deployment activate`.
func (c *cli) deploymentActivate(args []string) error {
	fs := newFlagSet("deployment activate")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, deploymentActivateHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 1, "duru --page <id> deployment activate <deploymentId> [flags]", &g, resPage); err != nil {
		return err
	}
	pageID, err := g.require(resPage)
	if err != nil {
		return err
	}
	depID := pos[0]
	path := pagePath(pageID) + "/deployments/" + url.PathEscape(depID) + "/activate"
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.request("POST", path, nil, "")
	if err != nil {
		return err
	}
	c.notef("deployment %s activated on page %q", depID, pageID)
	return c.printJSON(body)
}
