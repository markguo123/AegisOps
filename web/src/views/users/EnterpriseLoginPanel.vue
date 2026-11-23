<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { apiGet, apiPatch, apiPost } from '@/api/request'

interface Provider { enabled: boolean; issuer: string; clientId: string; clientSecret?: string; displayName: string; groupsClaim?: string; allowedGroups?: string[]; usernameClaim?: string; usernameClaimOverride?: boolean }
interface View {provider: Provider; hasSecret: boolean; callback: string}
const form = reactive<Provider>({ enabled: false, issuer: '', clientId: '', clientSecret: '', displayName: '', usernameClaim: '', usernameClaimOverride: false })
const groups = ref('')
const hasSecret = ref(false)
const callback = ref('')
const busy = ref(false)
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
</script>
<template>
  <el-form label-position="top" @submit.prevent="submit(false)">
    <el-form-item label="启用企业登录"><el-switch v-model="form.enabled" /></el-form-item>
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
