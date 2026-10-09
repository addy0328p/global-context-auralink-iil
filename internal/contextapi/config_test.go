package contextapi

import "testing"

func TestDemoConfigurationCannotWidenScope(t *testing.T) {
	_, _, valid := fixtureAPI(t)
	for _, mutation := range []func(*Config){
		func(c *Config) { c.ToolToken = c.DemoToken },
		func(c *Config) { c.DemoToken = "REPLACE_ME" },
		func(c *Config) { c.Subjects[0].Tenant = "another-tenant" },
		func(c *Config) { c.Subjects[0].Namespace = "live" },
		func(c *Config) { c.Subjects[0].Role = "admin" },
		func(c *Config) {
			duplicate := c.Subjects[0]
			duplicate.Namespace = "historical_dataset"
			c.Subjects = append(c.Subjects, duplicate)
		},
	} {
		c := valid
		c.Subjects = append(c.Subjects[:0:0], valid.Subjects...)
		mutation(&c)
		if c.Validate() == nil {
			t.Fatal("unsafe or ambiguous demo configuration accepted")
		}
	}
}
