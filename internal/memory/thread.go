package memory

import "errors"

// WithThread derives session slots from the published structured facts without
// rewriting the canonical Markdown, version, policy or content hash. A session
// chooses its thread explicitly; free-text facts remain unextracted.
func WithThread(c Context, thread string) (Context, error) {
	c.ActiveThreadRef = ""
	restrictions := cConstraints(c)
	c.CurrentConstraints = []Fact{}
	// Contact restrictions are subject-level constraints, regardless of thread.
	for _, f := range restrictions {
		c.CurrentConstraints = append(c.CurrentConstraints, f)
	}
	c.MissingFields = []string{}
	c.NextQuestion = "What would you like help with today?"
	if len(c.Requirements) > 0 {
		c.MissingFields = []string{"active_thread_ref"}
		c.NextQuestion = "Which saved requirement would you like to continue?"
	}
	if thread == "" {
		return c, nil
	}
	for _, work := range c.Requirements {
		if work.ThreadRef != thread {
			continue
		}
		c.ActiveThreadRef = thread
		c.MissingFields = []string{}
		known := map[string]bool{}
		for _, f := range work.Known {
			c.CurrentConstraints = append(c.CurrentConstraints, f)
			if f.Status != "conflict" {
				known[f.Field] = true
			}
		}
		for _, name := range []string{"product", "quantity", "unit", "delivery_city"} {
			if !known[name] {
				c.MissingFields = append(c.MissingFields, name)
			}
		}
		c.NextQuestion = "I have the saved details for this requirement. What would you like to update or continue?"
		for _, f := range work.Known {
			if f.Status == "conflict" {
				c.NextQuestion = "The saved requirement contains conflicting product details. Which value should we use?"
				break
			}
		}
		return c, nil
	}
	return Context{}, errors.New("selected thread has no supported requirement in this published role and window")
}

func cConstraints(c Context) []Fact {
	var facts []Fact
	for _, f := range c.CurrentConstraints {
		if f.Field == "do_not_call" {
			facts = append(facts, f)
		}
	}
	return facts
}
