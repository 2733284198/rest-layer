package schema

import (
	"fmt"
	"log"
	"reflect"
	"strings"

	"golang.org/x/net/context"
)

// Schema defines fields for a document
type Schema struct {
	// Description of the object described by this schema
	Description string
	// Fields defines the schema's allowed fields
	Fields Fields
}

// Validator is an interface used to validate schema against actual data
type Validator interface {
	GetField(name string) *Field
	Prepare(ctx context.Context, payload map[string]interface{}, original *map[string]interface{}, replace bool) (changes map[string]interface{}, base map[string]interface{})
	Validate(changes map[string]interface{}, base map[string]interface{}) (doc map[string]interface{}, errs map[string][]interface{})
}

// Compiler is an interface defining a validator that can be compiled at run time in order
// to check validator configuration validity and/or prepare some data for a faster execution.
type Compiler interface {
	Compile() error
}

// Serializer is an interface defining a validator able to serialize the data before validation
// and which provide a symetric deserializer.
type Serializer interface {
	Serialize(payload map[string]interface{}) error
}

type internal struct{}

// Tombstone is used to mark a field for removal
var Tombstone = internal{}

func addFieldError(errs map[string][]interface{}, field string, err interface{}) {
	errs[field] = append(errs[field], err)
}

func mergeFieldErrors(errs map[string][]interface{}, mergeErrs map[string][]interface{}) {
	// TODO recursive merge
	for field, values := range mergeErrs {
		if dest, found := errs[field]; found {
			for _, value := range values {
				dest = append(dest, value)
			}
		} else {
			errs[field] = values
		}
	}
}

// Compile implements Compiler interface and call the same function on each field
func (s Schema) Compile() error {
	// Search for all Dependecy on fields, and compile then
	if err := compileDependencies(s, s); err != nil {
		return err
	}
	for field, def := range s.Fields {
		// Compile each field
		if err := def.Compile(); err != nil {
			return fmt.Errorf("%s%v", field, err)
		}
	}
	return nil
}

// Serialize implements the Serializer interface by calling field serializer recusively
// on every fields
func (s Schema) Serialize(payload map[string]interface{}) error {
	for field, value := range payload {
		if def := s.GetField(field); def != nil {
			if def.Hidden {
				// If field is hidden, just remove it from the payload
				delete(payload, field)
			} else if s, ok := def.Validator.(FieldSerializer); ok {
				value, err := s.Serialize(value)
				if err != nil {
					return fmt.Errorf("%s: %s", field, err)
				}
				payload[field] = value
			}
			if def.Schema != nil {
				if sub, ok := value.(map[string]interface{}); ok {
					if err := def.Schema.Serialize(sub); err != nil {
						return fmt.Errorf("%s.%s", field, err)
					}
				}
			}
		}
	}
	return nil
}

// GetField returns the validator for the field if the given field name is present
// in the schema.
//
// You may reference sub field using dotted notation field.subfield
func (s Schema) GetField(name string) *Field {
	// Split the name to get the current level name on first element and
	// the rest of the path as second element if dot notation is used
	// (i.e.: field.subfield.subsubfield -> field, subfield.subsubfield)
	if i := strings.IndexByte(name, '.'); i != -1 {
		remaining := name[i+1:]
		name = name[:i]
		field, found := s.Fields[name]
		if !found {
			// Invalid node
			return nil
		}
		if field.Schema == nil {
			// Invalid path
			return nil
		}
		// Recursively call has field to consume the whole path
		return field.Schema.GetField(remaining)
	}
	if field, found := s.Fields[name]; found {
		return &field
	}
	return nil
}

// Prepare takes a payload with an optional original payout when updating an existing item and
// return two maps, one containing changes operated by the user and another defining either
// exising data (from the current item) or data generated by the system thru "default" value
// or hooks.
//
// If the original map is nil, prepare will act as if the payload is a new document. The OnInit
// hook is executed for each field if any, and default values are assigned to missing fields.
//
// When the original map is defined, the payload is considered as an update on the original document,
// default values are not assigned, and only fields which are different than in the original are
// left in the change map. The OnUpdate hook is executed on each field.
//
// If the replace argument is set to true with the original document set, the behavior is slighly
// different as any field not present in the payload but present in the original are set to nil
// in the change map (instead of just behing absent). This instruct the validator that the field
// has been edited, so ReadOnly flag can throw an error and the field will be removed from the
// output document. The OnInit is also called instead of the OnUpdate.
func (s Schema) Prepare(ctx context.Context, payload map[string]interface{}, original *map[string]interface{}, replace bool) (changes map[string]interface{}, base map[string]interface{}) {
	changes = map[string]interface{}{}
	base = map[string]interface{}{}
	for field, def := range s.Fields {
		value, found := payload[field]
		if original == nil {
			if replace == true {
				log.Panic("Cannot use replace=true without orignal")
			}
			// Handle prepare on a new document (no original)
			if !found || value == nil {
				// Add default fields
				if def.Default != nil {
					base[field] = def.Default
				}
			} else if found {
				changes[field] = value
			}
		} else {
			// Handle prepare on an updated document (original provided)
			oValue, oFound := (*original)[field]
			// Apply value to change-set only if the field was not identical same in the original doc
			if found && (!oFound || !reflect.DeepEqual(value, oValue)) {
				changes[field] = value
			}
			if !found && oFound && replace {
				// When replace arg is true and a field is not present in the payload but is in the original,
				// the tombstone value is set on the field in the change map so validator can enforce the
				// ReadOnly and then the field can be removed from the output document.
				// One exception to that though: if the field is set to hidden and is not readonly, we use
				// previous value as the client would have no way to resubmit the stored value.
				if def.Hidden && !def.ReadOnly {
					changes[field] = oValue
				} else {
					changes[field] = Tombstone
				}
			}
			if oFound {
				base[field] = oValue
			}
		}
		if def.Schema != nil {
			// Prepare sub-schema
			var subOriginal *map[string]interface{}
			if original != nil {
				// If original is provided, prepare the sub field if it exists and
				// is a dictionary. Otherwise, use an empty dict.
				oValue := (*original)[field]
				subOriginal = &map[string]interface{}{}
				if su, ok := oValue.(*map[string]interface{}); ok {
					subOriginal = su
				}
			}
			if found {
				if subPayload, ok := value.(map[string]interface{}); ok {
					// If payload contains a sub-document for this field, validate it
					// using the sub-validator
					c, b := def.Schema.Prepare(ctx, subPayload, subOriginal, replace)
					changes[field] = c
					base[field] = b
				} else {
					// Invalid payload, it will be caught by Validate()
				}
			} else {
				// If the payload doesn't contain a sub-document, perform validation
				// on an empty one so we don't miss default values
				c, b := def.Schema.Prepare(ctx, map[string]interface{}{}, subOriginal, replace)
				if len(c) > 0 || len(b) > 0 {
					// Only apply prepared field if something was added
					changes[field] = c
					base[field] = b
				}
			}
		}
		// Call the OnInit or OnUpdate depending on the presence of the original doc and the
		// state of the replace argument.
		var hook *func(ctx context.Context, value interface{}) interface{}
		if original == nil || replace {
			hook = def.OnInit
		} else {
			hook = def.OnUpdate
		}
		if hook != nil {
			// Get the change value or fallback on the base value
			if value, found := changes[field]; found {
				if value == Tombstone {
					// If the field has a tombstone, apply the handler on the base
					// and remove the tombstone so it doesn't appear as a user
					// generated change
					base[field] = (*hook)(ctx, base[field])
					delete(changes, field)
				} else {
					changes[field] = (*hook)(ctx, value)
				}
			} else {
				base[field] = (*hook)(ctx, base[field])
			}
		}
	}
	// Assign all out of schema fields to the changes map so Validate() can complain about it
	for field, value := range payload {
		if _, found := s.Fields[field]; !found {
			changes[field] = value
		}
	}
	return
}

// Validate validates changes applied on a base document in regard to the schema
// and generate an result document with the changes applied to the base document.
// All errors in the process are reported in the returned errs value.
func (s Schema) Validate(changes map[string]interface{}, base map[string]interface{}) (doc map[string]interface{}, errs map[string][]interface{}) {
	return s.validate(changes, base, true)
}
func (s Schema) validate(changes map[string]interface{}, base map[string]interface{}, isRoot bool) (doc map[string]interface{}, errs map[string][]interface{}) {
	doc = map[string]interface{}{}
	errs = map[string][]interface{}{}
	for field, def := range s.Fields {
		// Check read only fields
		if def.ReadOnly {
			if _, found := changes[field]; found {
				addFieldError(errs, field, "read-only")
			}
		}
		// Check required fields
		if def.Required {
			if value, found := changes[field]; !found || value == nil {
				if found {
					// If explicitely set to null, raise the required error
					addFieldError(errs, field, "required")
				} else if value, found = base[field]; !found || value == nil {
					// If field was omitted and isn't set by a Default of a hook, raise
					addFieldError(errs, field, "required")
				}
			}
		}
		// Validate sub-schema on non provided fields in order to enforce requireds
		if def.Schema != nil {
			if _, found := changes[field]; !found {
				if _, found := base[field]; !found {
					empty := map[string]interface{}{}
					if _, subErrs := def.Schema.validate(empty, empty, false); len(subErrs) > 0 {
						addFieldError(errs, field, subErrs)
					}
				}
			}
		}
	}
	// Apply changes to the base in doc
	for field, value := range base {
		doc[field] = value
	}
	for field, value := range changes {
		if value == Tombstone {
			// If the value is set for removal, remove it from the doc
			delete(doc, field)
		} else {
			doc[field] = value
		}
	}
	// Validate all dependency from the root schema only as dependencies can refers to parent schemas
	if isRoot {
		mergeErrs := s.validateDependencies(changes, doc, "")
		mergeFieldErrors(errs, mergeErrs)
	}
	for field, value := range doc {
		// Check invalid field (fields provided in the payload by not present in the schema)
		def, found := s.Fields[field]
		if !found {
			addFieldError(errs, field, "invalid field")
			continue
		}
		if def.Schema != nil {
			// Schema defines a sub-schema
			subChanges := map[string]interface{}{}
			subBase := map[string]interface{}{}
			// Check if changes contains a valid sub-document
			if v, found := changes[field]; found {
				if m, ok := v.(map[string]interface{}); ok {
					subChanges = m
				} else {
					addFieldError(errs, field, "not a dict")
				}
			}
			// Check if base contains a valid sub-document
			if v, found := base[field]; found {
				if m, ok := v.(map[string]interface{}); ok {
					subBase = m
				} else {
					addFieldError(errs, field, "not a dict")
				}
			}
			// Validate sub document and add the result to the current doc's field
			if subDoc, subErrs := def.Schema.validate(subChanges, subBase, false); len(subErrs) > 0 {
				addFieldError(errs, field, subErrs)
			} else {
				doc[field] = subDoc
			}
		} else if def.Validator != nil {
			// Apply validator if provided
			var err error
			if value, err = def.Validator.Validate(value); err != nil {
				addFieldError(errs, field, err.Error())
			} else {
				// Store the normalized value
				doc[field] = value
			}
		}
	}
	return doc, errs
}
