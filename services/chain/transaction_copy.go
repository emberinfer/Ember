package chain

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"reflect"
)

// copyMutable copies exported Go data without encoding consensus JSON. Protobuf
// messages use their own copier to avoid copying internal mutexes/caches.
func copyMutable(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	if v.Kind() == reflect.Pointer && !v.IsNil() && v.CanInterface() {
		if m, ok := v.Interface().(proto.Message); ok {
			cloned := proto.Clone(m)
			discardJSONUnknown(cloned.ProtoReflect())
			return reflect.ValueOf(cloned)
		}
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(copyMutable(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(copyMutable(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), copyMutable(it.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(copyMutable(v.Index(i)))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				out.Field(i).Set(copyMutable(v.Field(i)))
			}
		}
		return out
	default:
		return v
	}
}

// transactionCopy isolates all mutable fields. BlockHashes is immutable during
// Apply, so transactions share that large historical section. A block boundary
// copies its map before insertion. Legacy replay deliberately retains JSON Clone.
func (s *State) transactionCopy() (*State, error) {
	if s.Version < 11 {
		return s.Clone()
	}
	out := *s
	src, dst := reflect.ValueOf(s).Elem(), reflect.ValueOf(&out).Elem()
	for i := 0; i < src.NumField(); i++ {
		f := src.Type().Field(i)
		if f.IsExported() && f.Name != "BlockHashes" {
			dst.Field(i).Set(copyMutable(src.Field(i)))
		}
	}
	out.inferenceDebit = 0
	out.legacyAppHash = nil
	out.legacyHeight = 0
	return &out, nil
}

func (s *State) blockCopy() (*State, error) {
	out, err := s.transactionCopy()
	if err != nil {
		return nil, err
	}
	if s.Version >= 11 && s.BlockHashes != nil {
		out.BlockHashes = make(map[int64][]byte, len(s.BlockHashes)+1)
		for h, b := range s.BlockHashes {
			out.BlockHashes[h] = b
		}
	}
	return out, nil
}

// Apply already provides atomic publication, so v11 needs no outer per-tx copy.
func proposalTrial(s *State) (*State, error) {
	if s.Version >= 11 {
		return s, nil
	}
	return s.Clone()
}

// encoding/json (used by legacy Clone) does not retain protobuf unknown wire
// fields. Strip them recursively so intra-block copies match a JSON roundtrip.
func discardJSONUnknown(m protoreflect.Message) {
	m.SetUnknown(nil)
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.IsMap() {
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, x protoreflect.Value) bool { discardJSONUnknown(x.Message()); return true })
			}
		} else if fd.Message() != nil {
			if fd.IsList() {
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					discardJSONUnknown(l.Get(i).Message())
				}
			} else {
				discardJSONUnknown(v.Message())
			}
		}
		return true
	})
}
