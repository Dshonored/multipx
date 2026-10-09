//! Wire protocol — mirrors `internal/proto/proto.go` exactly. Field names
//! match the Go JSON tags one-for-one (both are already snake_case), so
//! there is no translation layer: this Rust client and the Go daemon speak
//! literally the same bytes.
//!
//! Framing: 4-byte big-endian length prefix + JSON body, on the control
//! stream for discovery/lifecycle, and once as a handshake on each freshly
//! opened attach stream before it becomes a raw byte pipe.

use anyhow::{bail, Result};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

pub const PROTOCOL_VERSION: i64 = 1;
const MAX_FRAME: u32 = 16 << 20;

#[derive(Debug, Serialize, Deserialize)]
pub struct Envelope {
    #[serde(rename = "type")]
    pub typ: String,
    #[serde(default)]
    pub payload: Value,
}

pub async fn write_frame<W: AsyncWriteExt + Unpin>(w: &mut W, v: &impl Serialize) -> Result<()> {
    let bytes = serde_json::to_vec(v)?;
    w.write_u32(bytes.len() as u32).await?;
    w.write_all(&bytes).await?;
    Ok(())
}

pub async fn read_frame<R: AsyncReadExt + Unpin>(r: &mut R) -> Result<Envelope> {
    let len = r.read_u32().await?;
    if len > MAX_FRAME {
        bail!("frame too large ({len} bytes)");
    }
    let mut buf = vec![0u8; len as usize];
    r.read_exact(&mut buf).await?;
    Ok(serde_json::from_slice(&buf)?)
}

pub async fn send<W: AsyncWriteExt + Unpin>(w: &mut W, typ: &str, payload: impl Serialize) -> Result<()> {
    let env = Envelope { typ: typ.to_string(), payload: serde_json::to_value(payload)? };
    write_frame(w, &env).await
}

#[derive(Debug, Serialize)]
pub struct HelloMsg {
    pub protocol_version: i64,
    pub client_name: String,
}

#[derive(Debug, Deserialize)]
pub struct HelloAckMsg {
    #[allow(dead_code)]
    pub protocol_version: i64,
    #[allow(dead_code)]
    pub daemon_id: String,
    #[allow(dead_code)]
    pub hostname: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SessionInfo {
    pub id: String,
    pub name: String,
    pub shell: String,
    pub status: String,
    #[serde(default)]
    pub exit_code: i64,
    pub created_at: String,
    pub last_active: String,
    pub attached: i64,
}

#[derive(Debug, Serialize)]
pub struct ListSessionsMsg {}

#[derive(Debug, Deserialize)]
pub struct SessionListMsg {
    pub sessions: Vec<SessionInfo>,
}

#[derive(Debug, Serialize)]
pub struct CreateSessionMsg {
    pub name: String,
    pub shell: String,
    pub cols: i64,
    pub rows: i64,
}

#[derive(Debug, Deserialize)]
pub struct SessionCreatedMsg {
    pub session: SessionInfo,
}

#[derive(Debug, Serialize)]
pub struct AttachMsg {
    pub session_id: String,
    pub cols: i64,
    pub rows: i64,
}

#[derive(Debug, Deserialize)]
pub struct AttachOkMsg {
    #[allow(dead_code)]
    pub session_id: String,
    pub scrollback_len: i64,
}

#[derive(Debug, Serialize)]
pub struct ResizeMsg {
    pub session_id: String,
    pub cols: i64,
    pub rows: i64,
}

#[derive(Debug, Serialize)]
pub struct KillSessionMsg {
    pub session_id: String,
}

#[derive(Debug, Serialize)]
pub struct RenameSessionMsg {
    pub session_id: String,
    pub name: String,
}

#[derive(Debug, Deserialize)]
pub struct ErrorMsg {
    pub message: String,
}
