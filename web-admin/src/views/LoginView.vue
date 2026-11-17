<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import type { Tenant } from '../api/types'
import { signIn } from '../state/session'

const router = useRouter()
const route = useRoute()

const email = ref('')
const password = ref('')
const tenantSlug = ref('')
const busy = ref(false)
const error = ref('')
const tenants = ref<Tenant[]>([])

async function submit(): Promise<void> {
  if (busy.value) return
  error.value = ''
  tenants.value = []
  if (!email.value.trim() || !password.value) {
    error.value = '请输入邮箱和密码'
    return
  }
  busy.value = true
  try {
    await signIn(email.value.trim(), password.value, tenantSlug.value.trim())
    const next = typeof route.query.next === 'string' ? route.query.next : '/workbench'
    await router.replace(next.startsWith('/') ? next : '/workbench')
  } catch (err) {
    if (err instanceof ApiError && err.status === 409 && err.code === 'tenant_selection_required') {
      tenants.value = err.problem.tenants ?? []
      error.value = '该账号属于多个工作室，请选择登录的工作室'
    } else if (err instanceof ApiError && err.status === 401) {
      error.value = '邮箱或密码不正确'
    } else if (err instanceof ApiError && err.status === 422) {
      error.value = err.detail || '请检查输入'
    } else {
      error.value = '登录失败，请稍后重试'
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
          <h1>登录工作室</h1>
          <p class="muted">摄影工作流管理端</p>
        </div>
      </div>

      <form class="login-form" @submit.prevent="submit">
        <label class="field">
          <span>邮箱</span>
          <input v-model="email" type="email" autocomplete="username" placeholder="owner@example.com" />
        </label>
        <label class="field">
          <span>密码</span>
          <input v-model="password" type="password" autocomplete="current-password" placeholder="••••••••" />
        </label>
        <label v-if="tenants.length === 0" class="field">
          <span>工作室标识（可选）</span>
          <input v-model="tenantSlug" type="text" autocomplete="organization" placeholder="studio-slug" />
        </label>
        <label v-else class="field">
          <span>工作室</span>
          <select v-model="tenantSlug">
            <option value="" disabled>请选择</option>
            <option v-for="tenant in tenants" :key="tenant.id" :value="tenant.slug">
              {{ tenant.name }}（{{ tenant.slug }}）
            </option>
          </select>
        </label>

        <p v-if="error" class="form-error" role="alert">{{ error }}</p>

        <button class="btn primary block" type="submit" :disabled="busy">
          {{ busy ? '登录中…' : '登录' }}
        </button>
      </form>

      <p class="login-foot muted small">还没有账号？<RouterLink to="/register">创建工作室</RouterLink></p>
      </div>
    </main>
  </div>
</template>
