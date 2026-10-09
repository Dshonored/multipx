mod client;
mod colors;
mod protocol;
mod render;

use std::collections::HashMap;
use std::sync::Arc;

use glyphon::{Attrs, Buffer, Cache, Family, FontSystem, Metrics, Shaping, SwashCache, TextAtlas, TextRenderer, Viewport};
use tokio::sync::mpsc;
use winit::application::ApplicationHandler;
use winit::event::{ElementState, Modifiers, MouseButton, WindowEvent};
use winit::event_loop::{ActiveEventLoop, EventLoop, EventLoopProxy};
use winit::keyboard::{Key, NamedKey};
use winit::window::{Window, WindowId};

use client::{AppEvent, Command};
use protocol::SessionInfo;

const SIDEBAR_W: f32 = 240.0;
const TOPBAR_H: f32 = 36.0;
const ROW_H: f32 = 24.0;
const FONT_SIZE: f32 = 15.0;
const LINE_HEIGHT: f32 = 19.0;

fn main() {
    let event_loop = EventLoop::<AppEvent>::with_user_event().build().unwrap();
    let proxy = event_loop.create_proxy();

    let (cmd_tx, cmd_rx) = mpsc::unbounded_channel::<Command>();
    let (event_tx, mut event_rx) = mpsc::unbounded_channel::<AppEvent>();

    let addr: std::net::SocketAddr = std::env::var("MULTIPX_ADDR")
        .unwrap_or_else(|_| "127.0.0.1:7890".to_string())
        .parse()
        .expect("invalid MULTIPX_ADDR");
    client::spawn_network_thread(addr, cmd_rx, event_tx);

    // Forward networking events into the winit event loop via the proxy, on
    // a tiny bridge thread, since mpsc::Receiver isn't itself pollable from
    // winit's loop.
    let bridge_proxy = proxy.clone();
    std::thread::spawn(move || {
        while let Some(ev) = event_rx.blocking_recv() {
            if bridge_proxy.send_event(ev).is_err() {
                return;
            }
        }
    });

    let mut app = App::new(proxy, cmd_tx);
    event_loop.run_app(&mut app).unwrap();
}

struct SessionView {
    info: SessionInfo,
    parser: Option<vt100::Parser>,
    attached: bool,
}

struct WindowState {
    device: wgpu::Device,
    queue: wgpu::Queue,
    surface: wgpu::Surface<'static>,
    surface_config: wgpu::SurfaceConfiguration,
    instance: wgpu::Instance,

    font_system: FontSystem,
    swash_cache: SwashCache,
    viewport: Viewport,
    atlas: TextAtlas,
    text_renderer: TextRenderer,

    sidebar_buffer: Buffer,
    term_buffer: Buffer,

    window: Arc<Window>,
}

impl WindowState {
    async fn new(window: Arc<Window>, event_loop: &ActiveEventLoop) -> Self {
        let size = window.inner_size();

        let instance = wgpu::Instance::new(wgpu::InstanceDescriptor::new_with_display_handle(
            Box::new(event_loop.owned_display_handle()),
        ));
        let adapter = instance
            .request_adapter(&wgpu::RequestAdapterOptions::default())
            .await
            .unwrap();
        let (device, queue) = adapter
            .request_device(&wgpu::DeviceDescriptor::default())
            .await
            .unwrap();

        let surface = instance.create_surface(window.clone()).unwrap();
        let format = wgpu::TextureFormat::Bgra8UnormSrgb;
        let surface_config = wgpu::SurfaceConfiguration {
            usage: wgpu::TextureUsages::RENDER_ATTACHMENT,
            format,
            width: size.width.max(1),
            height: size.height.max(1),
            present_mode: wgpu::PresentMode::Fifo,
            alpha_mode: wgpu::CompositeAlphaMode::Opaque,
            view_formats: vec![],
            desired_maximum_frame_latency: 2,
            color_space: wgpu::SurfaceColorSpace::Auto,
        };
        surface.configure(&device, &surface_config);

        let mut font_system = FontSystem::new();
        let swash_cache = SwashCache::new();
        let cache = Cache::new(&device);
        let viewport = Viewport::new(&device, &cache);
        let mut atlas = TextAtlas::new(&device, &queue, &cache, format);
        let text_renderer = TextRenderer::new(&mut atlas, &device, wgpu::MultisampleState::default(), None);

        let sidebar_buffer = Buffer::new(&mut font_system, Metrics::new(13.0, 18.0));
        let term_buffer = Buffer::new(&mut font_system, Metrics::new(FONT_SIZE, LINE_HEIGHT));

        Self {
            device,
            queue,
            surface,
            surface_config,
            instance,
            font_system,
            swash_cache,
            viewport,
            atlas,
            text_renderer,
            sidebar_buffer,
            term_buffer,
            window,
        }
    }

    /// Width in pixels of one monospace cell, measured by actually shaping
    /// a representative glyph rather than assumed from font size — this is
    /// exactly the mistake that made the Gio build's cell math drift.
    fn measure_cell(&mut self) -> (f32, f32) {
        let mut probe = Buffer::new(&mut self.font_system, Metrics::new(FONT_SIZE, LINE_HEIGHT));
        probe.set_text(
            "M",
            &Attrs::new().family(Family::Monospace),
            Shaping::Advanced,
            None,
        );
        probe.shape_until_scroll(&mut self.font_system, false);
        let w = probe
            .layout_runs()
            .next()
            .and_then(|run| run.glyphs.first())
            .map(|g| g.w)
            .unwrap_or(FONT_SIZE * 0.6);
        (w, LINE_HEIGHT)
    }
}

struct App {
    window_state: Option<WindowState>,
    proxy: EventLoopProxy<AppEvent>,
    cmd_tx: mpsc::UnboundedSender<Command>,

    sessions: Vec<SessionInfo>,
    views: HashMap<String, SessionView>,
    focused_id: String,
    modifiers: Modifiers,
    auto_create_tried: bool,
    status: String,

    sidebar_rows: Vec<(String, f32, f32)>, // id, row_top, row_h — for click hit-testing
    cursor_pos: (f32, f32),

    last_cols: i64,
    last_rows: i64,
}

impl App {
    fn new(proxy: EventLoopProxy<AppEvent>, cmd_tx: mpsc::UnboundedSender<Command>) -> Self {
        Self {
            window_state: None,
            proxy,
            cmd_tx,
            sessions: Vec::new(),
            views: HashMap::new(),
            focused_id: String::new(),
            modifiers: Modifiers::default(),
            auto_create_tried: false,
            status: "connecting…".into(),
            sidebar_rows: Vec::new(),
            cursor_pos: (0.0, 0.0),
            last_cols: 80,
            last_rows: 24,
        }
    }

    fn pane_cols_rows(&mut self) -> (i64, i64) {
        let Some(ws) = &mut self.window_state else { return (self.last_cols, self.last_rows) };
        let (cell_w, cell_h) = ws.measure_cell();
        let size = ws.window.inner_size();
        let pane_w = size.width as f32 - SIDEBAR_W - 16.0;
        let pane_h = size.height as f32 - TOPBAR_H - 28.0;
        let cols = ((pane_w / cell_w).floor() as i64).max(10);
        let rows = ((pane_h / cell_h).floor() as i64).max(3);
        (cols, rows)
    }

    fn focus(&mut self, id: String) {
        let (cols, rows) = self.pane_cols_rows();
        self.focused_id = id.clone();
        let already_attached = self.views.get(&id).map(|v| v.attached).unwrap_or(false);
        if already_attached {
            let _ = self.cmd_tx.send(Command::Resize(id, cols, rows));
        } else {
            let _ = self.cmd_tx.send(Command::Focus(id, cols, rows));
        }
    }

    fn redraw(&self) {
        if let Some(ws) = &self.window_state {
            ws.window.request_redraw();
        }
    }
}

impl ApplicationHandler<AppEvent> for App {
    fn resumed(&mut self, event_loop: &ActiveEventLoop) {
        if self.window_state.is_some() {
            return;
        }
        let attrs = Window::default_attributes()
            .with_inner_size(winit::dpi::LogicalSize::new(1280.0, 800.0))
            .with_title("multipx");
        let window = Arc::new(event_loop.create_window(attrs).unwrap());
        self.window_state = Some(pollster::block_on(WindowState::new(window, event_loop)));
        self.redraw();
    }

    fn user_event(&mut self, _event_loop: &ActiveEventLoop, event: AppEvent) {
        match event {
            AppEvent::SessionList(list) => {
                self.sessions = list.clone();
                for info in &list {
                    self.views
                        .entry(info.id.clone())
                        .and_modify(|v| v.info = info.clone())
                        .or_insert_with(|| SessionView { info: info.clone(), parser: None, attached: false });
                }
                let ids: std::collections::HashSet<_> = list.iter().map(|s| s.id.clone()).collect();
                self.views.retain(|id, _| ids.contains(id));

                if list.is_empty() && !self.auto_create_tried {
                    self.auto_create_tried = true;
                    let _ = self.cmd_tx.send(Command::CreateSession);
                } else if self.focused_id.is_empty() && !list.is_empty() {
                    self.focus(list[0].id.clone());
                }
            }
            AppEvent::Attached { id, scrollback_len } => {
                let (cols, rows) = (self.last_cols, self.last_rows);
                if let Some(v) = self.views.get_mut(&id) {
                    v.attached = true;
                    v.parser = Some(vt100::Parser::new(rows as u16, cols as u16, 2000));
                }
                self.status = format!("attached · replayed {scrollback_len} bytes");
            }
            AppEvent::Output { id, data } => {
                if let Some(v) = self.views.get_mut(&id) {
                    if let Some(p) = &mut v.parser {
                        p.process(&data);
                    }
                }
            }
            AppEvent::Closed { id } => {
                if let Some(v) = self.views.get_mut(&id) {
                    v.attached = false;
                }
            }
            AppEvent::Status(s) => self.status = s,
        }
        self.redraw();
    }

    fn window_event(&mut self, event_loop: &ActiveEventLoop, _window_id: WindowId, event: WindowEvent) {
        match event {
            WindowEvent::CloseRequested => event_loop.exit(),

            WindowEvent::Resized(size) => {
                if let Some(ws) = &mut self.window_state {
                    ws.surface_config.width = size.width.max(1);
                    ws.surface_config.height = size.height.max(1);
                    ws.surface.configure(&ws.device, &ws.surface_config);
                }
                let (cols, rows) = self.pane_cols_rows();
                self.last_cols = cols;
                self.last_rows = rows;
                if let Some(v) = self.views.get_mut(&self.focused_id) {
                    if let Some(p) = &mut v.parser {
                        p.screen_mut().set_size(rows as u16, cols as u16);
                    }
                }
                if !self.focused_id.is_empty() {
                    let _ = self.cmd_tx.send(Command::Resize(self.focused_id.clone(), cols, rows));
                }
                self.redraw();
            }

            WindowEvent::ModifiersChanged(m) => self.modifiers = m,

            WindowEvent::CursorMoved { position, .. } => {
                self.cursor_pos = (position.x as f32, position.y as f32);
            }

            WindowEvent::MouseInput { state: ElementState::Pressed, button: MouseButton::Left, .. } => {
                let (x, y) = self.cursor_pos;
                if x < SIDEBAR_W {
                    if let Some((id, _, _)) = self
                        .sidebar_rows
                        .iter()
                        .find(|(_, top, h)| y >= *top && y < *top + *h)
                        .cloned()
                    {
                        self.focus(id);
                    }
                }
            }

            WindowEvent::KeyboardInput { event, .. } => {
                if event.state != ElementState::Pressed || self.focused_id.is_empty() {
                    return;
                }
                let ctrl = self.modifiers.state().control_key();
                let bytes = key_to_bytes(&event, ctrl);
                if !bytes.is_empty() {
                    let _ = self.cmd_tx.send(Command::Input(self.focused_id.clone(), bytes));
                }
            }

            WindowEvent::RedrawRequested => self.draw(),

            _ => {}
        }
    }
}

/// Translates a winit key event into raw PTY bytes. `text` carries normal
/// typed characters (already layout/shift aware); named keys and Ctrl
/// combinations are handled explicitly since they produce no text.
fn key_to_bytes(event: &winit::event::KeyEvent, ctrl: bool) -> Vec<u8> {
    if ctrl {
        if let Key::Character(s) = &event.logical_key {
            if let Some(c) = s.chars().next() {
                let upper = c.to_ascii_uppercase();
                if upper.is_ascii_uppercase() {
                    return vec![upper as u8 - b'A' + 1];
                }
            }
        }
    }
    match &event.logical_key {
        Key::Named(NamedKey::Enter) => b"\r".to_vec(),
        Key::Named(NamedKey::Backspace) => vec![0x7f],
        Key::Named(NamedKey::Delete) => b"\x1b[3~".to_vec(),
        Key::Named(NamedKey::Tab) => b"\t".to_vec(),
        Key::Named(NamedKey::Escape) => vec![0x1b],
        Key::Named(NamedKey::Space) => b" ".to_vec(),
        Key::Named(NamedKey::ArrowUp) => b"\x1b[A".to_vec(),
        Key::Named(NamedKey::ArrowDown) => b"\x1b[B".to_vec(),
        Key::Named(NamedKey::ArrowRight) => b"\x1b[C".to_vec(),
        Key::Named(NamedKey::ArrowLeft) => b"\x1b[D".to_vec(),
        Key::Named(NamedKey::Home) => b"\x1b[H".to_vec(),
        Key::Named(NamedKey::End) => b"\x1b[F".to_vec(),
        Key::Named(NamedKey::PageUp) => b"\x1b[5~".to_vec(),
        Key::Named(NamedKey::PageDown) => b"\x1b[6~".to_vec(),
        _ => event.text.as_ref().map(|t| t.as_bytes().to_vec()).unwrap_or_default(),
    }
}
