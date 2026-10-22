package modelconfig

func towerOf(m map[string]any, keys ...string) *TowerConfig {
	for _, key := range keys {
		raw, ok := m[key].(map[string]any)
		if !ok {
			continue
		}
		return &TowerConfig{
			ModelType:             stringOf(raw, "model_type"),
			HiddenSize:            intOf(raw, "hidden_size", "encoder_hidden_size"),
			DModel:                intOf(raw, "d_model", "embed_dim"),
			IntermediateSize:      intOf(raw, "intermediate_size"),
			NumHiddenLayers:       intOf(raw, "num_hidden_layers", "num_layers"),
			EncoderLayers:         intOf(raw, "encoder_layers"),
			NumAttentionHeads:     intOf(raw, "num_attention_heads", "num_heads"),
			EncoderAttentionHeads: intOf(raw, "encoder_attention_heads"),
			ImageSize:             intOf(raw, "image_size", "image_sizes"),
			PatchSize:             intOf(raw, "patch_size"),
			NumChannels:           intOf(raw, "num_channels", "in_channels"),
			MaxPositionEmbeddings: intOf(raw, "max_position_embeddings", "num_position_embeddings"),
			MaxSourcePositions:    intOf(raw, "max_source_positions"),
			NumMelBins:            intOf(raw, "num_mel_bins"),
			ProjectionDim:         intOf(raw, "projection_dim"),
			OutputDim:             intOf(raw, "output_dim"),
			OutHiddenSize:         intOf(raw, "out_hidden_size"),
			EmbedDim:              intOf(raw, "embed_dim"),
			Depth:                 intOf(raw, "depth"),
			SpatialMergeSize:      intOf(raw, "spatial_merge_size"),
		}
	}
	return nil
}
