package dataset

import (
	"bufio"
	"encoding/json"
	"fmt"
	"sync"
	"unicode/utf8"
)

var (
	builtinProfilesOnce sync.Once
	builtinProfiles     map[string]Profile
	builtinProfilesErr  error
)

func BuiltinProfiles() (map[string]Profile, error) {
	builtinProfilesOnce.Do(func() {
		builtinProfiles = map[string]Profile{}
		builtinProfiles["multiturn"], builtinProfilesErr = conversationProfile()
		if builtinProfilesErr != nil {
			return
		}
	})
	out := make(map[string]Profile, len(builtinProfiles))
	for kind, profile := range builtinProfiles {
		out[kind] = profile
	}
	return out, builtinProfilesErr
}

func conversationProfile() (Profile, error) {
	f, err := openEmbedded("data/multiturn.jsonl")
	if err != nil {
		return Profile{}, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var profile Profile
	for scanner.Scan() {
		var row struct {
			Messages []Message `json:"messages"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return Profile{}, fmt.Errorf("decode conversation profile row: %w", err)
		}
		profile.SampleCount++
		for _, message := range row.Messages {
			tokens := approximateTokens(message.Content)
			switch message.Role {
			case "user":
				profile.MaxInputTokens = max(profile.MaxInputTokens, tokens)
			case "assistant":
				profile.MaxOutputTokens = max(profile.MaxOutputTokens, tokens)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func ProfileSamples(samples []Sample) Profile {
	profile := Profile{SampleCount: len(samples)}
	for _, sample := range samples {
		input := sample.InputTokens
		if input <= 0 {
			for _, message := range sample.Messages {
				input += approximateTokens(message.Content)
			}
		}
		profile.MaxInputTokens = max(profile.MaxInputTokens, input)
		profile.MaxOutputTokens = max(profile.MaxOutputTokens, sample.OutputTokens)
	}
	return profile
}

func approximateTokens(text string) int {
	runes := utf8.RuneCountInString(text)
	if runes <= 0 {
		return 0
	}
	return (runes + 3) / 4
}
