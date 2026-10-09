//! QUIC client — dials multipxd exactly like the Go CLI/TUI/GUI clients do,
//! over the same control-stream/attach-stream protocol. This file owns
//! nothing about rendering; it only talks to the daemon and exposes a
//! Command/Event channel pair to the UI thread.

use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{Context, Result};
use quinn::crypto::rustls::QuicClientConfig;
use quinn::{ClientConfig, Connection, Endpoint};
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{DigitallySignedStruct, SignatureScheme};
use tokio::sync::mpsc;

use crate::protocol::*;

const ALPN: &[u8] = b"multipx/1";

/// Accepts any server certificate. multipxd is loopback-only today and
/// generates a fresh self-signed cert per run (see transport.go's TODO on
/// real trust for remote hosts) — the Go client does the equivalent via
/// InsecureSkipVerify, so this matches it rather than introducing a
/// stricter posture the daemon doesn't support yet.
#[derive(Debug)]
struct NoVerify;

impl ServerCertVerifier for NoVerify {
    fn verify_server_cert(
        &self,
        _end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        Ok(ServerCertVerified::assertion())
    }
    fn verify_tls12_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        Ok(HandshakeSignatureValid::assertion())
    }
    fn verify_tls13_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        Ok(HandshakeSignatureValid::assertion())
    }
    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        rustls::crypto::ring::default_provider()
            .signature_verification_algorithms
            .supported_schemes()
    }
}

pub struct Client {
    conn: Connection,
    ctrl_send: quinn::SendStream,
    ctrl_recv: quinn::RecvStream,
}

impl Client {
    pub async fn dial(addr: SocketAddr) -> Result<Self> {
        let mut client_crypto = rustls::ClientConfig::builder_with_provider(
            rustls::crypto::ring::default_provider().into(),
        )
        .with_safe_default_protocol_versions()?
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(NoVerify))
        .with_no_client_auth();
        client_crypto.alpn_protocols = vec![ALPN.to_vec()];

        let quic_crypto = QuicClientConfig::try_from(client_crypto)?;
        let client_config = ClientConfig::new(Arc::new(quic_crypto));

        let mut endpoint = Endpoint::client("0.0.0.0:0".parse()?)?;
        endpoint.set_default_client_config(client_config);

        let conn = endpoint
            .connect(addr, "multipx-daemon")?
            .await
            .context("quic handshake")?;

        let (mut ctrl_send, mut ctrl_recv) = conn.open_bi().await?;
        send(&mut ctrl_send, "hello", HelloMsg {
            protocol_version: PROTOCOL_VERSION,
            client_name: "multipx-native".into(),
        })
        .await?;
        let env = read_frame(&mut ctrl_recv).await?;
        if env.typ != "hello_ack" {
            anyhow::bail!("unexpected handshake reply: {}", env.typ);
        }

        Ok(Self { conn, ctrl_send, ctrl_recv })
    }

    pub async fn list(&mut self) -> Result<Vec<SessionInfo>> {
        send(&mut self.ctrl_send, "list_sessions", ListSessionsMsg {}).await?;
        let env = read_frame(&mut self.ctrl_recv).await?;
        match env.typ.as_str() {
            "session_list" => Ok(serde_json::from_value::<SessionListMsg>(env.payload)?.sessions),
            "error" => anyhow::bail!(serde_json::from_value::<ErrorMsg>(env.payload)?.message),
            other => anyhow::bail!("unexpected reply: {other}"),
        }
    }

    pub async fn create(&mut self, name: &str, cols: i64, rows: i64) -> Result<SessionInfo> {
        send(&mut self.ctrl_send, "create_session", CreateSessionMsg {
            name: name.to_string(),
            shell: String::new(),
            cols,
            rows,
        })
        .await?;
        let env = read_frame(&mut self.ctrl_recv).await?;
        match env.typ.as_str() {
            "session_created" => Ok(serde_json::from_value::<SessionCreatedMsg>(env.payload)?.session),
            "error" => anyhow::bail!(serde_json::from_value::<ErrorMsg>(env.payload)?.message),
            other => anyhow::bail!("unexpected reply: {other}"),
        }
    }

    pub async fn kill(&mut self, id: &str) -> Result<()> {
        send(&mut self.ctrl_send, "kill_session", KillSessionMsg { session_id: id.to_string() }).await
    }

    pub async fn rename(&mut self, id: &str, name: &str) -> Result<()> {
        send(&mut self.ctrl_send, "rename_session", RenameSessionMsg {
            session_id: id.to_string(),
            name: name.to_string(),
        })
        .await
    }

    pub async fn resize(&mut self, id: &str, cols: i64, rows: i64) -> Result<()> {
        send(&mut self.ctrl_send, "resize", ResizeMsg { session_id: id.to_string(), cols, rows }).await
    }

    /// Opens a new stream and claims it for `id`. Returns the raw send/recv
    /// halves positioned right after the handshake — scrollback (if any)
    /// arrives as the first bytes of normal stream output, same as every
    /// other client.
    pub async fn attach(
        &self,
        id: &str,
        cols: i64,
        rows: i64,
    ) -> Result<(quinn::SendStream, quinn::RecvStream, i64)> {
        let (mut send_s, mut recv_s) = self.conn.open_bi().await?;
        send(&mut send_s, "attach", AttachMsg { session_id: id.to_string(), cols, rows }).await?;
        let env = read_frame(&mut recv_s).await?;
        match env.typ.as_str() {
            "attach_ok" => {
                let ok: AttachOkMsg = serde_json::from_value(env.payload)?;
                Ok((send_s, recv_s, ok.scrollback_len))
            }
            "error" => anyhow::bail!(serde_json::from_value::<ErrorMsg>(env.payload)?.message),
            other => anyhow::bail!("unexpected reply: {other}"),
        }
    }
}

/// Commands the UI thread sends to the networking task.
pub enum Command {
    CreateSession,
    KillSession(String),
    RenameSession(String, String),
    /// Attach if not already, or resize if already attached.
    Focus(String, i64, i64),
    Resize(String, i64, i64),
    Input(String, Vec<u8>),
}

/// Events the networking task sends back to the UI thread.
pub enum AppEvent {
    SessionList(Vec<SessionInfo>),
    Attached { id: String, scrollback_len: i64 },
    Output { id: String, data: Vec<u8> },
    Closed { id: String },
    Status(String),
}

/// Runs the whole networking side on a background Tokio runtime: owns the
/// control connection, polls the session list every 2s (matching the other
/// clients), and spawns one read-pump task per attached stream.
pub fn spawn_network_thread(
    addr: SocketAddr,
    cmd_rx: mpsc::UnboundedReceiver<Command>,
    event_tx: mpsc::UnboundedSender<AppEvent>,
) {
    std::thread::spawn(move || {
        let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
        rt.block_on(network_main(addr, cmd_rx, event_tx));
    });
}

async fn network_main(
    addr: SocketAddr,
    mut cmd_rx: mpsc::UnboundedReceiver<Command>,
    event_tx: mpsc::UnboundedSender<AppEvent>,
) {
    let mut client = match Client::dial(addr).await {
        Ok(c) => c,
        Err(e) => {
            let _ = event_tx.send(AppEvent::Status(format!("daemon unreachable: {e}")));
            return;
        }
    };

    let mut attach_streams: std::collections::HashMap<String, quinn::SendStream> =
        std::collections::HashMap::new();

    let mut poll = tokio::time::interval(std::time::Duration::from_secs(2));

    loop {
        tokio::select! {
            _ = poll.tick() => {
                if let Ok(list) = client.list().await {
                    let _ = event_tx.send(AppEvent::SessionList(list));
                }
            }
            cmd = cmd_rx.recv() => {
                let Some(cmd) = cmd else { return };
                match cmd {
                    Command::CreateSession => {
                        match client.create("", 80, 24).await {
                            Ok(info) => {
                                if let Ok(list) = client.list().await {
                                    let _ = event_tx.send(AppEvent::SessionList(list));
                                }
                                let _ = event_tx.send(AppEvent::Status(format!("created {}", info.name)));
                            }
                            Err(e) => { let _ = event_tx.send(AppEvent::Status(format!("create failed: {e}"))); }
                        }
                    }
                    Command::KillSession(id) => {
                        attach_streams.remove(&id);
                        let _ = client.kill(&id).await;
                        if let Ok(list) = client.list().await {
                            let _ = event_tx.send(AppEvent::SessionList(list));
                        }
                    }
                    Command::RenameSession(id, name) => {
                        let _ = client.rename(&id, &name).await;
                        if let Ok(list) = client.list().await {
                            let _ = event_tx.send(AppEvent::SessionList(list));
                        }
                    }
                    Command::Resize(id, cols, rows) => {
                        let _ = client.resize(&id, cols, rows).await;
                    }
                    Command::Input(id, data) => {
                        if let Some(s) = attach_streams.get_mut(&id) {
                            let _ = s.write_all(&data).await;
                        }
                    }
                    Command::Focus(id, cols, rows) => {
                        match client.attach(&id, cols, rows).await {
                            Ok((send_s, mut recv_s, scrollback_len)) => {
                                attach_streams.insert(id.clone(), send_s);
                                let _ = event_tx.send(AppEvent::Attached { id: id.clone(), scrollback_len });
                                let tx = event_tx.clone();
                                let read_id = id.clone();
                                tokio::spawn(async move {
                                    let mut buf = vec![0u8; 8 * 1024];
                                    loop {
                                        match recv_s.read(&mut buf).await {
                                            Ok(Some(n)) if n > 0 => {
                                                let _ = tx.send(AppEvent::Output {
                                                    id: read_id.clone(),
                                                    data: buf[..n].to_vec(),
                                                });
                                            }
                                            _ => {
                                                let _ = tx.send(AppEvent::Closed { id: read_id.clone() });
                                                return;
                                            }
                                        }
                                    }
                                });
                            }
                            Err(e) => {
                                let _ = event_tx.send(AppEvent::Status(format!("attach failed: {e}")));
                            }
                        }
                    }
                }
            }
        }
    }
}
