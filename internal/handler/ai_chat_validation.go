package handler

import "strings"

// validateSearchResult checks the model-generated conditions against the
// frontend-supplied searchFields + conditionRules. This is the structured
// validation the contract requires ("不能只依赖提示词"): the prompt steers the
// model, this function rejects anything that violates the machine rules.
//
// It returns the list of conditions that could not be safely mapped. An empty
// slice means the draft is valid.
func validateSearchResult(
	result *AISearchResult,
	searchFields []AISearchField,
	rules *AIConditionRules,
) (unmatched []AIUnmatched) {
	if result == nil {
		return nil
	}

	fields := make(map[string]AISearchField, len(searchFields))
	for _, f := range searchFields {
		fields[f.Name] = f
	}

	required := map[string]bool{}
	if rules != nil {
		for _, r := range rules.ResultShape.Required {
			required[r] = true
		}
	}

	for _, cond := range result.Conditions {
		f, ok := fields[cond.Field]
		if !ok {
			unmatched = append(unmatched, AIUnmatched{
				SourceText: cond.Field,
				Reason:     "no_matching_field",
			})
			continue
		}

		// Required-shape check.
		for _, r := range requiredFieldOrder(required) {
			if !conditionHasField(cond, r) {
				unmatched = append(unmatched, AIUnmatched{
					SourceText: cond.Field,
					Reason:     "missing_" + r,
				})
				break
			}
		}

		// Operator must be allowed for this field type.
		if rules != nil && !operatorAllowed(rules.OperatorsByType[f.Type], cond.Operator) {
			unmatched = append(unmatched, AIUnmatched{
				SourceText: cond.Field,
				Reason:     "invalid_operator",
			})
			continue
		}

		// Select values must come from the legal options.
		if f.Type == "select" && !selectValueAllowed(f.Options, cond.Value) {
			unmatched = append(unmatched, AIUnmatched{
				SourceText: cond.Field,
				Reason:     "invalid_value",
			})
			continue
		}
	}
	return unmatched
}

// operatorAllowed reports whether op is in the allowlist for a field type.
func operatorAllowed(allowed []string, op string) bool {
	for _, a := range allowed {
		if a == op {
			return true
		}
	}
	return false
}

// selectValueAllowed reports whether value is one of the select options' values.
func selectValueAllowed(options []AIFieldOption, value interface{}) bool {
	v, _ := value.(string)
	for _, o := range options {
		if o.Value == v {
			return true
		}
	}
	return false
}

// conditionHasField reports whether a condition carries a required field name
// with a non-empty value.
func conditionHasField(cond AICondition, field string) bool {
	switch field {
	case "field":
		return cond.Field != ""
	case "label":
		return cond.Label != ""
	case "type":
		return cond.Type != ""
	case "operator":
		return cond.Operator != ""
	case "value":
		return cond.Value != nil && strings.TrimSpace(valueString(cond.Value)) != ""
	default:
		return true
	}
}

func valueString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

// requiredFieldOrder returns the required fields in a stable order so the
// first missing one is reported deterministically.
func requiredFieldOrder(required map[string]bool) []string {
	order := []string{"field", "label", "type", "operator", "value"}
	out := make([]string, 0, len(order))
	for _, r := range order {
		if required[r] {
			out = append(out, r)
		}
	}
	return out
}
