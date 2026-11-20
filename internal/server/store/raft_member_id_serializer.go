package store

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	"gorm.io/gorm/schema"
)

// SQLite INTEGER is signed, while Raft member IDs span uint64. This
// serializer preserves the same 64 bits without changing the public field.
type raftMemberIDSerializer struct{}

func init() {
	schema.RegisterSerializer("raft_member_id", raftMemberIDSerializer{})
}

func (raftMemberIDSerializer) Value(_ context.Context, _ *schema.Field, _ reflect.Value, value any) (any, error) {
	id, ok := value.(uint64)
	if !ok {
		return nil, fmt.Errorf("invalid Raft member ID value %T", value)
	}
	return int64(id), nil
}

func (raftMemberIDSerializer) Scan(ctx context.Context, field *schema.Field, dst reflect.Value, value any) error {
	var bits int64
	switch v := value.(type) {
	case int64:
		bits = v
	case int:
		bits = int64(v)
	case []byte:
		parsed, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return err
		}
		bits = parsed
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return err
		}
		bits = parsed
	default:
		return fmt.Errorf("invalid stored Raft member ID type %T", value)
	}
	return field.Set(ctx, dst, uint64(bits))
}
