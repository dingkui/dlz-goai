package ollama

import (
	"reflect"
	"testing"

	"github.com/dingkui/dlz-goai/message"
)

func TestEncodeMessagesNormalizesImageDataURI(t *testing.T) {
	encoded := (codec{}).EncodeMessages([]message.Message{{
		Role: message.RoleUser, Images: []string{
			"data:image/png;base64,aGVsbG8=",
			"cmF3LWJhc2U2NA==",
			"https://example.test/image.png",
		},
	}})
	want := []string{"aGVsbG8=", "cmF3LWJhc2U2NA==", "https://example.test/image.png"}
	if got, ok := encoded[0]["images"].([]string); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Ollama 图片载荷不符: %#v", encoded[0]["images"])
	}
}
