curl -X POST http://localhost:3111/api/admin/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":             "all-minilm",
    "display_name":     "all-MiniLM-L6-v2 Embeddings",
    "service_type":     "EMBEDDING",
    "backend_type":     "vllm",
    "image":            "vllm/vllm-openai:latest",
    "host":             "0.0.0.0",
    "port":             8200,
    "node_id":          "341725ab-d93d-4d73-9a40-cd03bf137740",
    "hf_model_id":      "sentence-transformers/all-MiniLM-L6-v2",
    "execution_mode":   "gpu",
    "gpu_memory_util":  0.2,
    "start_now":        true,
    "capabilities":     ["embedding"]
  }'