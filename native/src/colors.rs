//! Color palette + vt100::Color -> RGB conversion. Unlike the Go/vt10x
//! version, vt100's `Color` enum has distinct `Idx`/`Rgb`/`Default`
//! variants — there is no overload ambiguity to work around here.

pub const SURFACE: (u8, u8, u8) = (0x24, 0x27, 0x2a);
pub const SURFACE_CONTAINER: (u8, u8, u8) = (0x30, 0x33, 0x36);
pub const TEXT_PRIMARY: (u8, u8, u8) = (0xdd, 0xde, 0xdf);
pub const TEXT_SECONDARY: (u8, u8, u8) = (0xbd, 0xc0, 0xc3);
pub const TEXT_DISABLED: (u8, u8, u8) = (0x74, 0x7a, 0x7e);
pub const PRIMARY: (u8, u8, u8) = (0x00, 0xd4, 0xd5);
pub const SUCCESS: (u8, u8, u8) = (0x00, 0xc9, 0x50);

const ANSI16: [(u8, u8, u8); 16] = [
    (0x1c, 0x1c, 0x1c),
    (0xe0, 0x6c, 0x75),
    (0x98, 0xc3, 0x79),
    (0xe5, 0xc0, 0x7b),
    (0x61, 0xaf, 0xef),
    (0xc6, 0x78, 0xdd),
    (0x56, 0xb6, 0xc2),
    (0xab, 0xb2, 0xbf),
    (0x5c, 0x63, 0x70),
    (0xe0, 0x6c, 0x75),
    (0x98, 0xc3, 0x79),
    (0xe5, 0xc0, 0x7b),
    (0x61, 0xaf, 0xef),
    (0xc6, 0x78, 0xdd),
    (0x56, 0xb6, 0xc2),
    (0xff, 0xff, 0xff),
];

const CUBE6: [u8; 6] = [0, 95, 135, 175, 215, 255];

pub fn fg_rgb(c: vt100::Color) -> (u8, u8, u8) {
    color_rgb(c, TEXT_PRIMARY)
}

pub fn bg_rgb(c: vt100::Color) -> Option<(u8, u8, u8)> {
    match c {
        vt100::Color::Default => None,
        other => Some(color_rgb(other, SURFACE)),
    }
}

fn color_rgb(c: vt100::Color, default: (u8, u8, u8)) -> (u8, u8, u8) {
    match c {
        vt100::Color::Default => default,
        vt100::Color::Rgb(r, g, b) => (r, g, b),
        vt100::Color::Idx(n) => idx_rgb(n),
    }
}

fn idx_rgb(n: u8) -> (u8, u8, u8) {
    match n {
        0..=15 => ANSI16[n as usize],
        16..=231 => {
            let m = n - 16;
            let r = CUBE6[(m / 36 % 6) as usize];
            let g = CUBE6[(m / 6 % 6) as usize];
            let b = CUBE6[(m % 6) as usize];
            (r, g, b)
        }
        232..=255 => {
            let v = 8 + (n - 232) * 10;
            (v, v, v)
        }
    }
}
