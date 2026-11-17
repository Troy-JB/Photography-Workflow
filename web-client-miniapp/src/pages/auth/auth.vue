<script setup lang="ts">
import { onShow } from '@dcloudio/uni-app'
import { ref } from 'vue'
import { getSession, login as wechatLoginRequest, loginWithPhone, requestPhoneCode } from '../../api'

const mode = ref<'login' | 'register'>('login')
const name = ref('')
const phone = ref('')
const verificationCode = ref('')
const devCode = ref('')
const phoneCode = ref('')
const error = ref('')
const busy = ref(false)

function enterClient(): void {
  uni.switchTab({ url: '/pages/index/index' })
}

async function sendCode(): Promise<void> {
  error.value = ''
  try {
    devCode.value = await requestPhoneCode(phone.value.trim())
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : '验证码暂时无法发送'
  }
}

async function submit(): Promise<void> {
  if (busy.value) return
  if (mode.value === 'register' && !name.value.trim()) {
    error.value = '请填写你的称呼'
    return
  }
  busy.value = true
  error.value = ''
  try {
    await loginWithPhone(name.value.trim(), phone.value.trim(), verificationCode.value.trim())
    enterClient()
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : '登录失败，请稍后重试'
  } finally {
    busy.value = false
  }
}

async function loginWithWechat(event: { detail?: { code?: string } }): Promise<void> {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    phoneCode.value = event.detail?.code ?? ''
    await wechatLoginRequest(name.value.trim(), phoneCode.value, phone.value.trim(), '')
    enterClient()
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : '微信授权登录失败'
  } finally {
    busy.value = false
  }
}

onShow(() => {
  if (getSession()) enterClient()
})
</script>

<template>
  <view class="auth-scene">
    <view class="auth-glow auth-glow-one" />
    <view class="auth-glow auth-glow-two" />
    <view class="auth-header">
      <view class="auth-wordmark">LUMEO<text class="auth-wordmark-dot">.</text></view>
      <text class="auth-header-note">YOUR MOMENTS, BEAUTIFULLY KEPT</text>
    </view>

    <view class="auth-content">
      <view class="auth-symbol" aria-hidden="true"><view class="auth-symbol-core" /><view class="auth-symbol-orbit" /></view>
      <text class="auth-kicker">CLIENT ACCOUNT</text>
      <text class="auth-title">{{ mode === 'login' ? '欢迎回来' : '加入 LUMEO' }}</text>
      <text class="auth-subtitle">{{ mode === 'login' ? '登录后，继续你的影像故事。' : '创建账号，珍藏每一个值得的瞬间。' }}</text>

      <view class="auth-form">
        <view class="auth-tabs" role="tablist" aria-label="登录或注册">
          <button :class="['auth-tab', { active: mode === 'login' }]" :aria-selected="mode === 'login'" @click="mode = 'login'; error = ''">登录</button>
          <button :class="['auth-tab', { active: mode === 'register' }]" :aria-selected="mode === 'register'" @click="mode = 'register'; error = ''">注册</button>
        </view>
        <view v-if="mode === 'register'" class="auth-field">
          <text class="auth-label">怎么称呼你</text>
          <input v-model="name" class="auth-input" placeholder="请输入姓名" autocomplete="name" />
        </view>
        <view class="auth-field">
          <text class="auth-label">手机号码</text>
          <input v-model="phone" class="auth-input" type="tel" placeholder="请输入手机号" autocomplete="tel" />
        </view>
        <view class="auth-field">
          <text class="auth-label">验证码</text>
          <view class="auth-code-row">
            <input v-model="verificationCode" class="auth-input" type="number" placeholder="请输入短信验证码" autocomplete="one-time-code" />
            <button class="auth-code-button" @click="sendCode">获取验证码</button>
          </view>
        </view>
        <text v-if="devCode" class="auth-dev-code">开发验证码：{{ devCode }}</text>
        <view v-if="error" class="auth-error" role="alert">{{ error }}</view>
        <button class="auth-submit" :disabled="busy" @click="submit">{{ busy ? '正在验证…' : mode === 'login' ? '登录 LUMEO' : '创建账号' }}</button>
        <!-- #ifdef MP-WEIXIN -->
        <button class="auth-wechat" :disabled="busy" open-type="getPhoneNumber" @getphonenumber="loginWithWechat">微信快捷登录</button>
        <!-- #endif -->
        <text class="auth-privacy">登录即表示你同意服务条款与隐私政策</text>
      </view>
    </view>

    <view class="auth-footer"><view class="auth-footer-rule" /><text>以光为引，记录真实的你</text><view class="auth-footer-rule" /></view>
  </view>
</template>

<style>
.auth-scene { position: relative; display: flex; flex-direction: column; align-items: center; justify-content: space-between; min-height: 100vh; min-height: 100svh; overflow: hidden; padding: max(28px, env(safe-area-inset-top)) 24px max(20px, env(safe-area-inset-bottom)); color: #372720; background: radial-gradient(ellipse at 16% 8%, rgb(255 255 255 / 94%) 0%, rgb(255 255 255 / 0%) 42%), radial-gradient(ellipse at 88% 27%, rgb(240 163 111 / 34%) 0%, rgb(240 163 111 / 0%) 41%), radial-gradient(ellipse at 62% 102%, rgb(167 70 44 / 17%) 0%, rgb(167 70 44 / 0%) 47%), linear-gradient(135deg, #f7f0e9 0%, #f0e4d8 48%, #fbf5ee 100%); }
.auth-glow { position: absolute; z-index: 0; width: min(54vw, 440px); aspect-ratio: 1; border-radius: 50%; filter: blur(2px); pointer-events: none; }
.auth-glow-one { top: 12%; right: -20%; background: radial-gradient(circle, rgb(255 235 215 / 68%), rgb(255 235 215 / 0%) 70%); }
.auth-glow-two { bottom: 4%; left: -24%; background: radial-gradient(circle, rgb(217 141 105 / 20%), rgb(217 141 105 / 0%) 70%); }
.auth-header, .auth-content, .auth-footer { position: relative; z-index: 1; width: 100%; }
.auth-header { display: flex; flex-direction: column; align-items: center; gap: 7px; }
.auth-wordmark { color: #35251e; font-size: 23px; font-weight: 700; letter-spacing: .06em; }
.auth-wordmark-dot { color: #a7462c; }
.auth-header-note { color: #8c7568; font-size: 9px; letter-spacing: .16em; }
.auth-content { display: flex; flex-direction: column; align-items: center; width: min(100%, 420px); margin: auto 0; padding: 42px 0 48px; text-align: center; }
.auth-symbol { position: relative; display: grid; place-items: center; width: 66px; height: 66px; margin-bottom: 20px; border: 1px solid rgb(167 70 44 / 20%); border-radius: 50%; background: radial-gradient(circle at 35% 28%, #fffdf9 0%, #f7dfcd 54%, #ecc4a7 100%); box-shadow: 0 10px 28px rgb(126 74 47 / 12%), inset 0 1px 0 rgb(255 255 255 / 90%); }
.auth-symbol-core { width: 21px; height: 21px; border: 1px solid #a7462c; border-radius: 50%; background: rgb(255 255 255 / 52%); }
.auth-symbol-orbit { position: absolute; width: 36px; height: 36px; border: 1px solid rgb(167 70 44 / 58%); border-radius: 50%; transform: rotate(-35deg) scaleX(.62); }
.auth-kicker { color: #967b6c; font-size: 9px; letter-spacing: .2em; }
.auth-title { display: block; margin-top: 10px; color: #36251f; font-size: 28px; font-weight: 600; line-height: 1.35; }
.auth-subtitle { display: block; margin-top: 6px; color: #806b5d; font-size: 13px; line-height: 1.7; }
.auth-form { width: 100%; margin-top: 26px; padding: 20px 22px 18px; border: 1px solid rgb(255 255 255 / 76%); border-radius: 8px; background: rgb(255 252 248 / 70%); box-shadow: 0 16px 44px rgb(91 57 40 / 8%), inset 0 1px 0 rgb(255 255 255 / 80%); backdrop-filter: blur(14px); }
.auth-tabs { display: grid; grid-template-columns: 1fr 1fr; margin-bottom: 15px; border-bottom: 1px solid rgb(120 83 63 / 20%); }
.auth-tab { width: 100%; min-height: 44px; padding: 0; border: 0; border-bottom: 2px solid transparent; border-radius: 0; color: #8a7468; background: transparent; font-size: 14px; line-height: 44px; transition: color .18s ease, border-color .18s ease; }
.auth-tab::after { border: 0; }
.auth-tab.active { border-bottom-color: #a7462c; color: #382820; font-weight: 600; }
.auth-field { display: grid; gap: 6px; margin-top: 12px; text-align: left; }
.auth-label { color: #614b3e; font-size: 12px; }
.auth-input { min-width: 0; width: 100%; height: 48px; padding: 0 13px; border: 1px solid #dfd0c4; border-radius: 4px; color: #372720; background: rgb(255 255 255 / 86%); font-size: 15px; }
.auth-input::placeholder { color: #a08c7e; }
.auth-code-row { display: flex; gap: 8px; }
.auth-code-button { flex: 0 0 auto; min-height: 48px; padding: 0 12px; border: 1px solid #c6a18b; border-radius: 4px; color: #70432f; background: #fffaf5; font-size: 12px; }
.auth-dev-code { display: block; margin-top: 7px; color: #8c4d36; font-size: 11px; text-align: left; }
.auth-error { margin-top: 10px; padding: 8px 10px; border-left: 2px solid #ad473e; color: #87392f; background: #fbebe6; font-size: 12px; text-align: left; }
.auth-submit { width: 100%; min-height: 50px; margin-top: 18px; border: 0; border-radius: 4px; color: #fffaf5; background: #382820; box-shadow: 0 5px 14px rgb(56 40 32 / 12%); font-size: 14px; font-weight: 600; transition: opacity .18s ease, transform .18s ease; }
.auth-submit[disabled] { opacity: .58; }
.auth-submit:active, .auth-code-button:active, .auth-tab:active { opacity: .78; }
.auth-wechat { width: 100%; min-height: 46px; margin-top: 9px; border: 1px solid #d6c4b7; border-radius: 4px; color: #59463a; background: rgb(255 255 255 / 60%); font-size: 13px; }
.auth-privacy { display: block; margin-top: 14px; color: #8d786b; font-size: 10px; line-height: 1.6; }
.auth-footer { display: flex; align-items: center; justify-content: center; gap: 12px; color: #8c7568; font-size: 11px; }
.auth-footer-rule { width: 25px; height: 1px; background: rgb(140 117 104 / 42%); }
@media (min-width: 760px) { .auth-content { padding-top: 30px; padding-bottom: 30px; } .auth-form { padding: 22px 26px 20px; } }
@media (max-width: 390px) { .auth-scene { padding-right: 18px; padding-left: 18px; } .auth-content { padding-top: 28px; padding-bottom: 28px; } .auth-form { padding-right: 16px; padding-left: 16px; } .auth-title { font-size: 26px; } }
@media (max-height: 720px) { .auth-scene { padding-top: max(18px, env(safe-area-inset-top)); padding-bottom: max(14px, env(safe-area-inset-bottom)); } .auth-content { padding-top: 18px; padding-bottom: 18px; } .auth-symbol { width: 52px; height: 52px; margin-bottom: 12px; } .auth-form { margin-top: 18px; padding-top: 14px; padding-bottom: 14px; } .auth-tabs { margin-bottom: 10px; } .auth-field { margin-top: 8px; } }
@media (prefers-reduced-motion: reduce) { .auth-scene * { transition: none !important; } }
</style>
