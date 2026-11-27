package provider

// Compatibility APIs take text tool results. Deliver their images in a user
// image turn AFTER all results in that round, so parallel calls keep their IDs
// and no tool result is orphaned. This preserves images across later rounds.
func toolImageMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	var images []Part
	flush := func() {
		if len(images) > 0 {
			out = append(out, Message{Role: "user", Content: "Images opened by view_image for your private inspection (not displayed to the user).", Parts: append([]Part{{Type: PartText, Text: "Images opened by view_image for your private inspection (not displayed to the user)."}}, images...)})
			images = nil
		}
	}
	for _, m := range messages {
		if m.Role != "tool" {
			flush()
		}
		if m.Role == "tool" && len(m.Parts) > 0 {
			for _, part := range m.Parts {
				if part.Type == PartImage {
					images = append(images, part)
				}
			}
			m.Parts = nil
		}
		out = append(out, m)
	}
	flush()
	return out
}
