package cli

// commandAgent groups the device-side commands: this machine as a
// Fleet-managed device.
type commandAgent struct {
	enroll  commandAgentEnroll
	run     commandAgentRun     // Task 16
	install commandAgentInstall // Task 17
	status  commandAgentStatus
}

func (c *commandAgent) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("agent", "WarpHold agent: this machine as a Fleet-managed device.")
	c.enroll.setup(svc, cmd)
	c.run.setup(svc, cmd)
	c.install.setup(svc, cmd)
	c.status.setup(svc, cmd)
}
