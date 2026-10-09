use glyphon::{Attrs, Color as GColor, Family, Resolution, Shaping, TextArea, TextBounds, Weight};

use crate::colors;
use crate::App;

impl App {
    pub(crate) fn draw(&mut self) {
        self.sidebar_rows.clear();

        // --- sidebar rich text -------------------------------------------------
        let mut sidebar_runs: Vec<String> = Vec::new();
        for s in &self.sessions {
            let dot = if s.status == "running" { "● " } else { "○ " };
            sidebar_runs.push(format!("{dot}{}\n", s.name));
        }
        let sidebar_spans: Vec<(&str, Attrs)> = sidebar_runs
            .iter()
            .zip(&self.sessions)
            .map(|(text, s)| {
                let focused = s.id == self.focused_id;
                let (r, g, b) = if focused { colors::PRIMARY } else { colors::TEXT_PRIMARY };
                let mut attrs = Attrs::new().family(Family::SansSerif).color(GColor::rgb(r, g, b));
                if focused {
                    attrs = attrs.weight(Weight::BOLD);
                }
                (text.as_str(), attrs)
            })
            .collect();

        // --- terminal rich text --------------------------------------------------
        let mut term_runs: Vec<String> = Vec::new();
        let mut term_colors: Vec<(u8, u8, u8)> = Vec::new();
        let mut rows_visible = 0u16;
        let mut cols_visible = 0u16;

        if let Some(v) = self.views.get(&self.focused_id) {
            if let Some(parser) = &v.parser {
                let screen = parser.screen();
                let (rows, cols) = screen.size();
                rows_visible = rows;
                cols_visible = cols;
                for row in 0..rows {
                    let mut run = String::new();
                    let mut run_color = colors::TEXT_PRIMARY;
                    let mut have_run = false;
                    for col in 0..cols {
                        let Some(cell) = screen.cell(row, col) else { continue };
                        let fg = colors::fg_rgb(cell.fgcolor());
                        let ch = if cell.has_contents() { cell.contents() } else { " " };
                        if !have_run || fg != run_color {
                            if have_run {
                                term_runs.push(std::mem::take(&mut run));
                                term_colors.push(run_color);
                            }
                            run_color = fg;
                            have_run = true;
                        }
                        run.push_str(ch);
                    }
                    if have_run {
                        run.push('\n');
                        term_runs.push(run);
                        term_colors.push(run_color);
                    } else {
                        term_runs.push("\n".to_string());
                        term_colors.push(colors::TEXT_PRIMARY);
                    }
                }
            }
        }
        let term_spans: Vec<(&str, Attrs)> = term_runs
            .iter()
            .zip(&term_colors)
            .map(|(text, (r, g, b))| {
                (text.as_str(), Attrs::new().family(Family::Monospace).color(GColor::rgb(*r, *g, *b)))
            })
            .collect();

        let status_line = self.status.clone();
        let session_count = self.sessions.len();

        let Some(ws) = &mut self.window_state else { return };
        let size = ws.window.inner_size();
        let scale = ws.window.scale_factor() as f32;

        ws.sidebar_buffer.set_rich_text(
            sidebar_spans.into_iter(),
            &Attrs::new().family(Family::SansSerif).color(GColor::rgb(
                colors::TEXT_PRIMARY.0,
                colors::TEXT_PRIMARY.1,
                colors::TEXT_PRIMARY.2,
            )),
            Shaping::Advanced,
            None,
        );
        ws.sidebar_buffer.set_size(Some(SIDEBAR_W_PX - 16.0), Some(size.height as f32));
        ws.sidebar_buffer.shape_until_scroll(&mut ws.font_system, false);

        ws.term_buffer.set_rich_text(
            term_spans.into_iter(),
            &Attrs::new().family(Family::Monospace).color(GColor::rgb(
                colors::TEXT_PRIMARY.0,
                colors::TEXT_PRIMARY.1,
                colors::TEXT_PRIMARY.2,
            )),
            Shaping::Advanced,
            None,
        );
        let term_w = size.width as f32 - SIDEBAR_W_PX - 16.0;
        ws.term_buffer.set_size(Some(term_w.max(1.0)), Some(size.height as f32));
        ws.term_buffer.shape_until_scroll(&mut ws.font_system, false);

        // Record sidebar row rectangles for click hit-testing, in logical
        // (unscaled) coordinates to match CursorMoved's reporting.
        let row_h_logical = 18.0 * 1.0; // matches sidebar Metrics line height
        for (i, s) in self.sessions.iter().enumerate() {
            let top = (8.0 + i as f32 * row_h_logical) / scale.max(1.0);
            self.sidebar_rows.push((s.id.clone(), top, row_h_logical / scale.max(1.0)));
        }

        ws.viewport.update(&ws.queue, Resolution { width: ws.surface_config.width, height: ws.surface_config.height });

        let areas = vec![
            TextArea {
                buffer: &ws.sidebar_buffer,
                left: 8.0 * scale,
                top: 40.0 * scale,
                scale,
                bounds: TextBounds { left: 0, top: 0, right: (SIDEBAR_W_PX * scale) as i32, bottom: ws.surface_config.height as i32 },
                default_color: GColor::rgb(colors::TEXT_PRIMARY.0, colors::TEXT_PRIMARY.1, colors::TEXT_PRIMARY.2),
                custom_glyphs: &[],
            },
            TextArea {
                buffer: &ws.term_buffer,
                left: (SIDEBAR_W_PX + 16.0) * scale,
                top: 40.0 * scale,
                scale,
                bounds: TextBounds { left: 0, top: 0, right: ws.surface_config.width as i32, bottom: ws.surface_config.height as i32 },
                default_color: GColor::rgb(colors::TEXT_PRIMARY.0, colors::TEXT_PRIMARY.1, colors::TEXT_PRIMARY.2),
                custom_glyphs: &[],
            },
        ];

        let _ = (session_count, rows_visible, cols_visible, status_line);

        if ws
            .text_renderer
            .prepare(&ws.device, &ws.queue, &mut ws.font_system, &mut ws.atlas, &ws.viewport, areas, &mut ws.swash_cache)
            .is_err()
        {
            return;
        }

        let frame = match ws.surface.get_current_texture() {
            wgpu::CurrentSurfaceTexture::Success(f) => f,
            wgpu::CurrentSurfaceTexture::Timeout | wgpu::CurrentSurfaceTexture::Occluded => {
                ws.window.request_redraw();
                return;
            }
            wgpu::CurrentSurfaceTexture::Outdated | wgpu::CurrentSurfaceTexture::Suboptimal(_) => {
                ws.surface.configure(&ws.device, &ws.surface_config);
                ws.window.request_redraw();
                return;
            }
            wgpu::CurrentSurfaceTexture::Lost => {
                ws.surface = ws.instance.create_surface(ws.window.clone()).unwrap();
                ws.surface.configure(&ws.device, &ws.surface_config);
                ws.window.request_redraw();
                return;
            }
            wgpu::CurrentSurfaceTexture::Validation => return,
        };
        let view = frame.texture.create_view(&wgpu::TextureViewDescriptor::default());
        let mut encoder = ws.device.create_command_encoder(&wgpu::CommandEncoderDescriptor { label: None });
        {
            let mut pass = encoder.begin_render_pass(&wgpu::RenderPassDescriptor {
                label: None,
                color_attachments: &[Some(wgpu::RenderPassColorAttachment {
                    view: &view,
                    depth_slice: None,
                    resolve_target: None,
                    ops: wgpu::Operations {
                        load: wgpu::LoadOp::Clear(wgpu::Color { r: 0.141, g: 0.153, b: 0.165, a: 1.0 }),
                        store: wgpu::StoreOp::Store,
                    },
                })],
                depth_stencil_attachment: None,
                timestamp_writes: None,
                occlusion_query_set: None,
                multiview_mask: None,
            });
            let _ = ws.text_renderer.render(&ws.atlas, &ws.viewport, &mut pass);
        }
        ws.queue.submit(Some(encoder.finish()));
        ws.queue.present(frame);
        ws.atlas.trim();
    }
}

const SIDEBAR_W_PX: f32 = 240.0;
