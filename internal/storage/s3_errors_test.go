package storage

import (
	"errors"
	"testing"

	"github.com/aws/smithy-go"
)

// apiError fakes a provider failure with the given code.
type apiError struct{ code string }

func (e apiError) Error() string                 { return "provider: " + e.code }
func (e apiError) ErrorCode() string             { return e.code }
func (e apiError) ErrorMessage() string          { return e.code }
func (e apiError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

// statusError fakes a bare HTTP failure like a HEAD with no body.
type statusError struct{ status int }

func (e statusError) Error() string       { return "http: unexpected status" }
func (e statusError) HTTPStatusCode() int { return e.status }

func TestMapS3Err(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"nil stays nil", nil, nil},
		{"NoSuchKey is absence", apiError{"NoSuchKey"}, ErrNotExist},
		{"NotFound is absence", apiError{"NotFound"}, ErrNotExist},
		{"bare 404 is absence", statusError{404}, ErrNotExist},
		{"AccessDenied is permission", apiError{"AccessDenied"}, ErrPermission},
		{"bad key is permission", apiError{"InvalidAccessKeyId"}, ErrPermission},
		{"expired token is permission", apiError{"ExpiredToken"}, ErrPermission},
		{"conflict exists", apiError{"Conflict"}, ErrExist},
		{"precondition exists", apiError{"PreconditionFailed"}, ErrExist},
		{"unmapped passes through", apiError{"SlowDown"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mapS3Err(tc.err)
			if tc.want == nil {
				// Nil in, nil out; unmapped errors come back identical.
				if tc.err == nil && got != nil {
					t.Fatalf("mapS3Err(nil) = %v, want nil", got)
				}
				if tc.err != nil && got != tc.err {
					t.Fatalf("mapS3Err(%v) = %v, want it untouched", tc.err, got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapS3Err(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
