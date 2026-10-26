#[derive(Debug, Clone)]
pub struct SpringValue {
    pub cur: f32,
    pub target: f32,
    pub vel: f32,
}

impl SpringValue {
    pub const K: f32 = 148.0;      // 对应 Pulse response: 0.5s (ω ≈ 2π/0.5)
    pub const DAMP: f32 = 16.8;    // 对应 Pulse damping: 0.85 (欠阻尼微过冲)
    pub const EPS: f32 = 0.0005;

    pub fn new(val: f32) -> Self {
        Self {
            cur: val,
            target: val,
            vel: 0.0,
        }
    }

    pub fn set(&mut self, target: f32) {
        self.target = target;
    }

    pub fn step(&mut self, dt: f32) -> bool {
        if dt <= 0.0 || dt > 0.1 {
            self.cur = self.target;
            self.vel = 0.0;
            return false;
        }

        let diff = self.target - self.cur;
        self.vel += (diff * Self::K - self.vel * Self::DAMP) * dt;
        self.cur += self.vel * dt;

        if (self.target - self.cur).abs() < Self::EPS && self.vel.abs() < Self::EPS * 10.0 {
            self.cur = self.target;
            self.vel = 0.0;
            return false;
        }

        true
    }

    pub fn is_at_rest(&self) -> bool {
        (self.target - self.cur).abs() < 1e-6 && self.vel.abs() < 1e-6
    }
}

#[derive(Debug, Clone)]
pub struct FadeValue {
    pub cur: f32,
    pub target: f32,
    pub speed: f32,
}

impl FadeValue {
    pub fn new(val: f32, speed: f32) -> Self {
        Self {
            cur: val,
            target: val,
            speed,
        }
    }

    pub fn set(&mut self, target: f32) {
        self.target = target;
    }

    pub fn step(&mut self, dt: f32) -> bool {
        if (self.target - self.cur).abs() < 0.001 {
            self.cur = self.target;
            return false;
        }
        let dir = if self.target > self.cur { 1.0 } else { -1.0 };
        self.cur += dir * self.speed * dt;
        if (dir > 0.0 && self.cur > self.target) || (dir < 0.0 && self.cur < self.target) {
            self.cur = self.target;
            return false;
        }
        true
    }
}

#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct SpinValue {
    pub phase: f32,
    pub period: f32,
    pub active: bool,
}

#[allow(dead_code)]
impl SpinValue {
    pub fn new(period: f32) -> Self {
        Self {
            phase: 0.0,
            period,
            active: false,
        }
    }

    pub fn start(&mut self) {
        self.active = true;
    }

    pub fn stop(&mut self) {
        self.active = false;
        self.phase = 0.0;
    }

    pub fn step(&mut self, dt: f32) -> bool {
        if !self.active || self.period <= 0.0 {
            return false;
        }
        self.phase += dt / self.period;
        if self.phase >= 1.0 {
            self.phase -= self.phase.floor();
        }
        true
    }
}
