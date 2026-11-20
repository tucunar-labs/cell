package contracts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	tucunarv1 "github.com/tucunar/contracts/gen/tucunar/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestCondorReadRequestGoldenFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("fixtures", "tucunar", "v1", "condor-read-request.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var request tucunarv1.ReadRequest
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(fixture, &request); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if request.Context == nil || request.Context.OrganizationId != "org-golden" || request.Consumer != "canary-evaluator" || request.Limit != 1000 {
		t.Fatalf("decoded fixture = %#v", &request)
	}
	if request.Since == nil || request.Until == nil || !request.Since.AsTime().Before(request.Until.AsTime()) {
		t.Fatalf("fixture bounds = since=%v until=%v", request.Since, request.Until)
	}
	canonical, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(&request)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	if !json.Valid(canonical) {
		t.Fatal("canonical fixture is not JSON")
	}
	// protojson cambia el espaciado entre versiones, así que se compara el JSON
	// semántico: detecta drift real del contrato, no del formateo.
	var wantJSON, gotJSON any
	if err := json.Unmarshal(fixture, &wantJSON); err != nil {
		t.Fatalf("decode fixture as JSON: %v", err)
	}
	if err := json.Unmarshal(canonical, &gotJSON); err != nil {
		t.Fatalf("decode canonical as JSON: %v", err)
	}
	if !reflect.DeepEqual(wantJSON, gotJSON) {
		t.Fatalf("golden fixture drifted:\nwant:\n%s\ngot:\n%s", fixture, canonical)
	}
	wire, err := proto.Marshal(&request)
	if err != nil {
		t.Fatalf("marshal protobuf: %v", err)
	}
	var roundTrip tucunarv1.ReadRequest
	if err := proto.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatalf("unmarshal protobuf: %v", err)
	}
	if !proto.Equal(&request, &roundTrip) {
		t.Fatal("protobuf wire round-trip changed the request")
	}
}

func TestCondorReadRequestFieldNumbersAreAdditive(t *testing.T) {
	fields := tucunarv1.File_tucunar_v1_condor_proto.Messages().ByName("ReadRequest").Fields()
	want := map[protoreflect.Name]protoreflect.FieldNumber{
		"context":  1,
		"stream":   2,
		"cursor":   3,
		"limit":    4,
		"until":    5,
		"consumer": 6,
		"since":    7,
	}
	for name, number := range want {
		field := fields.ByName(name)
		if field == nil || field.Number() != number {
			t.Fatalf("ReadRequest.%s number = %v, want %v", name, fieldNumber(field), number)
		}
	}
	if fields.Len() != len(want) {
		t.Fatalf("ReadRequest field count = %d, want %d", fields.Len(), len(want))
	}
}

func fieldNumber(field protoreflect.FieldDescriptor) protoreflect.FieldNumber {
	if field == nil {
		return 0
	}
	return field.Number()
}
