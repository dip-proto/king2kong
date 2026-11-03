//go:build kingpinv2

package kongcompat

// UsageForContextWithTemplate prints the usage of the command ctx reached,
// with a different template and indentation (kingpin v2).
func (a *Application) UsageForContextWithTemplate(ctx *ParseContext, indent int, template string) error {
	uc := UsageContext{}
	if a.usage != nil {
		uc = *a.usage
	}
	uc.Indent, uc.Template = indent, template
	return a.usageFor(ctx, &uc)
}

// Model returns the command's data model (kingpin v2).
func (c *CmdClause) Model() *CmdModel {
	return c.model()
}
