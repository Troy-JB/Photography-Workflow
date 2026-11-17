<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import { signUp } from '../state/session'

const router = useRouter()
const route = useRoute()
const accountKind = ref<'photographer' | 'client'>('photographer')
const name = ref('')
const email = ref('')
const password = ref('')
const tenantName = ref('')
const busy = ref(false)
const error = ref('')

onMounted(() => {
  if (route.query.for === 'client') accountKind.value = 'client'
})

async function submit(): Promise<void> {
  if (busy.value) return
  error.value = ''
  busy.value = true
  try {
    await signUp(name.value.trim(), email.value.trim(), password.value, tenantName.value.trim())
    await router.replace('/workbench')
  } catch (err) {
    if (err instanceof ApiError && err.status === 409 && err.code === 'email_already_registered') {
      error.value = '此邮箱已注册，请直接登录'
    } else if (err instanceof ApiError && err.status === 422) {
      error.value = '请检查姓名、邮箱、密码和工作室名称'
    } else if (err instanceof ApiError && err.status >= 500) {
      error.value = '服务暂时不可用，请稍后重试；如持续失败，请联系管理员'
    } else if (err instanceof TypeError) {
      error.value = '无法连接注册服务，请检查网络或联系管理员确认 API 地址配置'
    } else {
      error.value = '注册失败，请稍后重试'
    }
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <aside class="auth-visual" aria-label="Lumeo">
      <RouterLink class="auth-wordmark" to="/">Lumeo<span>.</span></RouterLink>
      <div class="auth-copy">
        <span class="auth-kicker">STUDIO WORKFLOW</span>
        <h2>把光影留给创作，<br />把流程交给 Lumeo。</h2>
      </div>
      <RouterLink class="auth-home" to="/">返回官网 ↗</RouterLink>
    </aside>

    <main class="auth-main">
      <div class="login-card">
      <div class="login-brand">
        <span class="brand-mark lg">P</span>
        <div>
          <h1>{{ accountKind === 'photographer' ? '创建工作室' : '客户账号' }}</h1>
          <p class="muted">{{ accountKind === 'photographer' ? '摄影师管理端' : '微信小程序客户中心' }}</p>
        </div>
      </div>

      <div class="role-tabs" role="tablist" aria-label="选择账号类型">
        <button type="button" role="tab" :aria-selected="accountKind === 'photographer'" :class="{ active: accountKind === 'photographer' }" @click="accountKind = 'photographer'">摄影师</button>
        <button type="button" role="tab" :aria-selected="accountKind === 'client'" :class="{ active: accountKind === 'client' }" @click="accountKind = 'client'">客户</button>
      </div>

      <form v-if="accountKind === 'photographer'" class="login-form" @submit.prevent="submit">
        <label class="field">
          <span>姓名</span>
          <input v-model="name" type="text" autocomplete="name" maxlength="120" required />
        </label>
        <label class="field">
          <span>邮箱</span>
          <input v-model="email" type="email" autocomplete="email" maxlength="254" required />
        </label>
        <label class="field">
          <span>密码</span>
          <input v-model="password" type="password" autocomplete="new-password" minlength="8" maxlength="128" required />
          <small class="muted">至少 8 位</small>
        </label>
        <label class="field">
          <span>工作室名称</span>
          <input v-model="tenantName" type="text" autocomplete="organization" maxlength="120" required />
        </label>

        <p v-if="error" class="form-error" role="alert">{{ error }}</p>

        <button class="btn primary block" type="submit" :disabled="busy">
          {{ busy ? '正在创建…' : '注册并创建工作室' }}
        </button>
      </form>

      <div v-else class="client-registration">
        <h2>客户从摄影师邀请进入</h2>
        <p>请在微信中打开摄影师发来的预约链接，登录并授权手机号后，会自动关联到对应工作室。</p>
        <p class="client-registration-note">客户账号不会创建工作室，也不会进入摄影师管理端。</p>
        <RouterLink class="btn primary block client-login-link" to="/login">摄影师管理端登录</RouterLink>
      </div>

      <p v-if="accountKind === 'photographer'" class="login-foot muted small">已有账号？<RouterLink to="/login">返回登录</RouterLink></p>
      </div>
    </main>
  </div>
</template>
