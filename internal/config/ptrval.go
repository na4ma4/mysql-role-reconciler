package config

type ptrVal[T any] struct {
	val *T
}

func (p *ptrVal[T]) Set(v T) {
	p.val = &v
}

func (p ptrVal[T]) Get() T {
	if p.val == nil {
		var zero T
		return zero
	}

	return *p.val
}

func (p ptrVal[T]) IsSet() bool {
	return p.val != nil
}

func (p *ptrVal[T]) UnmarshalYAML(unmarshal func(any) error) error {
	var v T
	if err := unmarshal(&v); err != nil {
		return err
	}
	p.val = &v
	return nil
}

// MarshalYAML returns the underlying value when set. When unset, returns (nil, nil)
// which tells the YAML encoder to omit the field — this is the correct behavior.
//
//nolint:nilnil // (nil, nil) is intentional: signals "omit" to yaml.v3 marshaler
func (p *ptrVal[T]) MarshalYAML() (any, error) {
	if p.val == nil {
		return nil, nil
	}

	return *p.val, nil
}
