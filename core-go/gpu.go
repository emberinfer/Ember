package core

import "strings"

func IsGPUExecutionProfile(version string) bool {
	return version == "transformers-qwen2.5-gpu-v1" || version == "transformers-smollm2-gpu-v1" || strings.HasPrefix(version, "transformers-qwen2.5-pipeline-gpu")
}
