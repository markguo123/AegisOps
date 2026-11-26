<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { apiGet, apiPatch, apiPost } from '@/api/request'

interface Provider { enabled: boolean; issuer: string; clientId: string; clientSecret?: string; displayName: string; groupsClaim?: string; allowedGroups?: string[]; usernameClaim?: string; usernameClaimOverride?: boolean }
interface View {provider: Provider; hasSecret: boolean; callback: string}
interface PortalConfig {
  enabled: boolean
  localDisabled: boolean
  homeUrl: string
  checkTokenUrl: string
}
interface PortalView { portal: PortalConfig; entry: string }
const form = reactive<Provider>({ enabled: false, issuer: '', clientId: '', clientSecret: '', displayName: '', usernameClaim: '', usernameClaimOverride: false })
const groups = ref('')
const hasSecret = ref(false)
const callback = ref('')
const busy = ref(false)
const portal = ref<PortalConfig>()
const portalEntry = ref('')
const portalBusy = ref(false)
const portalError = ref('')

async function loadPortal() {
  try {
    const { data } = await apiGet<PortalView>('/settings/auth/portal')
    if (!data) throw new Error('无法读取效能平台登录配置')
    portal.value = data.portal
    portalEntry.value = data.entry
  } catch (e) {
    portalError.value = e instanceof Error ? e.message : '无法读取效能平台登录配置'
  }
}

async function savePortal() {
  if (!portal.value) return
  portalBusy.value = true
  portalError.value = ''
  try {
    const { data } = await apiPatch<PortalView>('/settings/auth/portal', portal.value)
    if (!data) throw new Error('无法保存登录方式')
    portal.value = data.portal
    portalEntry.value = data.entry
    ElMessage.success('登录方式已保存')
  } catch (e) {
    portalError.value = e instanceof Error ? e.message : '无法保存登录方式'
  } finally {
    portalBusy.value = false
  }
}
async function load() {
  const { data } = await apiGet<View>('/settings/auth')
  if (data) { Object.assign(form, data.provider, {clientSecret: ''}); hasSecret.value = data.hasSecret; callback.value=data.callback; groups.value=(data.provider.allowedGroups ?? []).join(', ') }
}
async function submit(test: boolean) {
  busy.value=true
  try {
    const payload={...form, allowedGroups:groups.value.split(',').map(s=>s.trim()).filter(Boolean)}
    if(test) await apiPost('/settings/auth/test',payload)
    else {await apiPatch('/settings/auth',payload);await load()}
    ElMessage.success(test?'发现服务连接成功':'企业登录设置已保存')
  } catch(e){ElMessage.error(e instanceof Error?e.message:'配置失败')} finally {busy.value=false}
}
onMounted(()=>load().catch(()=>ElMessage.error('无法读取企业登录配置')))
onMounted(loadPortal)
</script>
<template>
  <el-form v-if="portal" label-position="top" @submit.prevent="savePortal">
    <el-form-item label="允许本地账号密码登录">
      <el-switch v-model="portal.localDisabled" :active-value="false" :inactive-value="true" />
    </el-form-item>
    <el-form-item label="启用效能平台登录"><el-switch v-model="portal.enabled" /></el-form-item>
    <el-form-item label="效能平台首页"><el-input v-model="portal.homeUrl" /></el-form-item>
    <el-form-item label="Token 校验地址"><el-input v-model="portal.checkTokenUrl" placeholder="留空时使用效能平台默认校验路径" /></el-form-item>
    <el-form-item label="效能平台应用入口地址">{{ portalEntry || '请先设置 platform.server.public_url' }}</el-form-item>
    <el-button type="primary" native-type="submit" :loading="portalBusy">保存本地与效能平台设置</el-button>
  </el-form>
  <el-alert v-if="portalError" :title="portalError" type="error" :closable="false" />
  <el-divider content-position="left">OIDC</el-divider>
  <el-form label-position="top" @submit.prevent="submit(false)">
    <el-form-item label="启用 OIDC 登录"><el-switch v-model="form.enabled" /></el-form-item>
    <el-form-item label="登录入口名称"><el-input v-model="form.displayName" /></el-form-item>
    <el-form-item label="Issuer URL"><el-input v-model="form.issuer" /></el-form-item>
    <el-form-item label="Client ID"><el-input v-model="form.clientId" /></el-form-item>
    <el-form-item label="Client Secret"><el-input v-model="form.clientSecret" type="password" :placeholder="hasSecret?'已设置，留空保留原值':''" autocomplete="new-password" /></el-form-item>
    <el-form-item label="回调地址">{{ callback || '请先设置 platform.server.public_url' }}</el-form-item>
    <el-form-item label="用户组 Claim"><el-input v-model="form.groupsClaim" placeholder="groups" /></el-form-item>
    <el-form-item label="准入用户组（可选，逗号分隔）"><el-input v-model="groups" /></el-form-item>
    <el-form-item label="登录名 Claim"><el-input v-model="form.usernameClaim" placeholder="preferred_username" /></el-form-item>
    <el-form-item label="强制使用该 Claim"><el-switch v-model="form.usernameClaimOverride" /></el-form-item>
    <el-button :loading="busy" @click="submit(true)">测试连接</el-button>
    <el-button type="primary" native-type="submit" :loading="busy">保存</el-button>
  </el-form>
</template>
