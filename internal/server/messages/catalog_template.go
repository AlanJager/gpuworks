package messages

// Saving a deployment as an inference template
// (POST /api/v1/deployments/{id}/catalog-template, see
// docs/how-to/save-deployment-as-template.md).
const (
	// ErrorTemplateExportUnavailable reports a deployment that cannot be
	// saved as a template; params.reason says why.
	ErrorTemplateExportUnavailable = "TEMPLATE_EXPORT_UNAVAILABLE"
	// ErrorLaunchNotExportable reports a launch configuration whose command
	// line cannot be reproduced from catalog args ({engine}).
	ErrorLaunchNotExportable = "LAUNCH_NOT_EXPORTABLE"
)
