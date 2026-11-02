package inferenceapi

import (
	"testing"

	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

func TestMetadataToOperations(t *testing.T) {
	tests := []struct {
		name, config, readme, want string
	}{
		{"ranking with generation backbone", `{"architectures":["Qwen3ForCausalLM"]}`, "---\npipeline_tag: text-ranking\n---\n", OperationRerank},
		{"reranker tag", `{}`, "\n---\npipeline_tag: text-classification\ntags: [reranker]\n---\n", OperationRerank},
		{"cross encoder label map", `{"architectures":["XLMRobertaForSequenceClassification"],"id2label":{"0":"LABEL_0"}}`, "---\ntags: [sentence-transformers]\n---\n", OperationRerank},
		{"cross encoder label count", `{"architectures":["BertForSequenceClassification"],"num_labels":1}`, "---\ntags: [sentence-transformers]\n---\n", OperationRerank},
		{"multiclass is not ranking", `{"architectures":["BertForSequenceClassification"],"num_labels":2,"id2label":{"0":"LABEL_0"}}`, "---\ntags: [sentence-transformers]\n---\n", ""},
		{"scalar regression is not ranking", `{"architectures":["BertForSequenceClassification"],"num_labels":1}`, "---\npipeline_tag: text-classification\n---\n", ""},
		{"unknown label count", `{"architectures":["BertForSequenceClassification"]}`, "---\ntags: [sentence-transformers]\n---\n", ""},
		{"feature extraction", `{}`, "---\npipeline_tag: feature-extraction\n---\n", OperationEmbeddings},
		{"sentence similarity", `{}`, "---\npipeline_tag: sentence-similarity\n---\n", OperationEmbeddings},
		{"feature extraction tag", `{}`, "---\ntags: [feature-extraction]\n---\n", OperationEmbeddings},
		{"ranking tag wins generic embedding tag", `{}`, "---\ntags: [reranker, feature-extraction]\n---\n", OperationRerank},
		{"declared task wins tag", `{}`, "---\npipeline_tag: text-generation\ntags: [feature-extraction]\n---\n", OperationChat},
		{"CRLF and BOM", `{}`, "\ufeff---\r\npipeline_tag: text-ranking\r\n---\r\n", OperationRerank},
		{"README prose is not a declaration", `{}`, "# Reranker\nExample:\n---\npipeline_tag: text-ranking\n---\n", ""},
		{"truncated card", `{}`, "---\npipeline_tag: text-ranking\n", ""},
		{"malformed card", `{}`, "---\ntags: [broken\n---\n", ""},
		{"text to image", `{}`, "---\npipeline_tag: text-to-image\n---\n", OperationImageGenerate},
		{"image to image edits a source image", `{}`, "---\npipeline_tag: image-to-image\n---\n", OperationImageEdit},
		{"ordinary chat", `{"architectures":["Qwen2ForCausalLM"]}`, "# Chat", OperationChat},
		{"no metadata", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OperationsForTasks(modelmetadata.InferTasks(modelmetadata.Documents{Config: tt.config, README: tt.readme}).Tasks)
			if (tt.want == "" && len(got) != 0) || (tt.want != "" && (len(got) != 1 || got[0].Type != tt.want)) {
				t.Fatalf("operations = %+v, want %s", got, tt.want)
			}
		})
	}
}
