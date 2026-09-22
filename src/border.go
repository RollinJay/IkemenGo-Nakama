package main

func (m *Motif) drawBorderLayer(layer BorderLayerProperties) {
	if layer.AnimData == nil && layer.Text.TextSpriteData == nil && layer.Overlay.RectData == nil {
		return
	}
	if layer.AnimData != nil {
		layer.AnimData.Update(false)
	}
	if layer.Text.TextSpriteData != nil {
		layer.Text.TextSpriteData.Update()
	}
	if layer.Overlay.RectData != nil {
		layer.Overlay.RectData.Update()
	}
	for ln := int16(0); ln <= 3; ln++ {
		if layer.AnimData != nil {
			layer.AnimData.Draw(ln)
		}
		if layer.Text.TextSpriteData != nil {
			layer.Text.TextSpriteData.Draw(ln)
		}
		if layer.Overlay.RectData != nil {
			layer.Overlay.RectData.Draw(ln)
		}
	}
}

func (m *Motif) drawBorderBackground() {
	if !sys.presentationEnabled() || !m.Border.Enabled {
		return
	}
	m.drawBorderLayer(m.Border.Background)
}

func (m *Motif) drawBorderForeground() {
	if !sys.presentationEnabled() || !m.Border.Enabled {
		return
	}
	m.drawBorderLayer(m.Border.Foreground)
}
