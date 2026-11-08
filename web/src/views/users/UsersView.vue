<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  Delete,
  Edit,
  Lock,
  Plus,
  Refresh,
  Unlock,
  User,
} from '@element-plus/icons-vue'

import EnterpriseLoginPanel from './EnterpriseLoginPanel.vue'
import TechEmptyState from '@/components/TechEmptyState.vue'
import {
  createGrant,
  createUser,
  deleteGrant,
  deleteUser,
  describeUsersError,
  listGrants,
  listRoles,
  listUsers,
  updateGrant,
  updateUser,
} from '@/api/users'
import type {
  AuthRole,
  AuthUser,
  AuthzGrant,
  CreateGrantRequest,
  CreateUserRequest,
  GrantPermission,
  GrantPrincipalType,
  UpdateUserRequest,
} from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import { hasPermission, PERM, permissionsOf } from '@/utils/permission'

import {listClusters} from '@/api/clusters'
const registered=ref<Awaited<ReturnType<typeof listClusters>>>([])
const auth = useAuthStore()

type SectionKey = 'users' | 'roles' | 'grants' | 'enterprise'
const SECTION_META: Array<{ key: SectionKey; title: string; desc: string }> = [
  { key: 'users', title: '用户管理', desc: '账号、角色与状态' },
  { key: 'roles', title: '角色与权限', desc: '两角色权限矩阵' },
  { key: 'grants', title: '授权委派', desc: 'namespace 授权与销毁委派' },
  { key: 'enterprise', title: '企业登录', desc: 'OIDC 单点登录与准入' },
]
const activeSection = ref<SectionKey>('users')

/** P3.11 后端全部用户/权限端点要求 user.manage，前端只隐藏无权入口。 */
const canManage = computed(() => hasPermission(permissionsOf(auth.role), PERM.userManage))

const users = ref<AuthUser[]>([])
const roles = ref<AuthRole[]>([])
const grants = ref<AuthzGrant[]>([])
const loading = ref(false)
const ROLE_ORDER: Record<string, number> = { user: 0, admin: 2 }

function normalizedRoles(list: AuthRole[]): AuthRole[] {
  return list
    .filter((role) => ROLE_ORDER[role.name] !== undefined)
    .map((role) => ({
      ...role,
      permissions: Array.isArray(role.permissions) ? role.permissions : [],
    }))
    .sort((a, b) => ROLE_ORDER[a.name]! - ROLE_ORDER[b.name]!)
}

function defaultUserRoleID(): string {
  return roles.value.find((role) => role.name === 'user')?.id ?? roles.value[0]?.id ?? ''
}

const currentUserId = computed(() => auth.user?.userId ?? '')

function formatTime(value?: string): string {
  if (!value) {
    return '—'
  }
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) {
    return value
  }
  return date.toLocaleString()
}

function roleName(id?: string): string {
  return roles.value.find((r) => r.id === id)?.name ?? id ?? '—'
}

function userStatusType(status?: string): 'success' | 'danger' | 'warning' | 'info' {
  switch (status) {
    case 'active':
      return 'success'
    case 'disabled':
      return 'warning'
    default:
      return 'info'
  }
}

function userStatusText(status?: string): string {
  switch (status) {
    case 'active':
      return '正常'
    case 'disabled':
      return '停用'
    default:
      return status || '未知'
  }
}

function principalTypeText(type?: string): string {
  return type === 'group' ? '组' : '用户'
}

function grantPermissionText(permission: string): string {
  return permission
}

function escapeNamespaces(list: string[]): string {
  return list.length ? list.join(' / ') : '—'
}

async function loadAll(): Promise<void> {
  loading.value = true
  try {
    const [userList, roleList, grantList] = await Promise.all([listUsers(), listRoles(), listGrants()])
    users.value = userList
    roles.value = normalizedRoles(roleList)
    if (!roles.value.some((role) => role.id === userForm.roleId)) {
      userForm.roleId = defaultUserRoleID()
    }
    grants.value = grantList

  } catch (error) {
    ElMessage.error(describeUsersError('加载用户与权限', error))
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  await loadAll()
  try{registered.value=await listClusters()}catch{registered.value=[]}
})

/** ---------------- 用户管理 ---------------- */

const userForm = reactive({
  visible: false,
  mode: 'create' as 'create' | 'edit',
  id: '',
  username: '',
  displayName: '',
  password: '',
  roleId: '',
  status: 'active',
})
const savingUser = ref(false)

function resetUserForm(): void {
  userForm.visible = false
  userForm.mode = 'create'
  userForm.id = ''
  userForm.username = ''
  userForm.displayName = ''
  userForm.password = ''
  userForm.roleId = defaultUserRoleID()
  userForm.status = 'active'
}

function openCreateUser(): void {
  userForm.mode = 'create'
  userForm.id = ''
  userForm.username = ''
  userForm.displayName = ''
  userForm.password = ''
  userForm.roleId = defaultUserRoleID()
  userForm.status = 'active'
  userForm.visible = true
}

function openEditUser(target: AuthUser): void {
  userForm.mode = 'edit'
  userForm.id = target.id
  userForm.username = target.username
  userForm.displayName = target.displayName ?? ''
  userForm.password = ''
  userForm.roleId = roles.value.some((role) => role.id === target.roleId)
    ? (target.roleId ?? defaultUserRoleID())
    : defaultUserRoleID()
  userForm.status = target.status === 'disabled' ? 'disabled' : 'active'
  userForm.visible = true
}

async function submitUser(): Promise<void> {
  const username = userForm.username.trim()
  if (!username) {
    ElMessage.warning('请输入用户名')
    return
  }
  if (userForm.mode === 'create' && userForm.password.length < 8) {
    ElMessage.warning('密码长度不能少于 8 位')
    return
  }
  if (userForm.mode === 'edit' && userForm.password && userForm.password.length < 8) {
    ElMessage.warning('新密码长度不能少于 8 位')
    return
  }
  if (!userForm.roleId) {
    ElMessage.warning('请选择角色')
    return
  }
  savingUser.value = true
  try {
    if (userForm.mode === 'create') {
      const req: CreateUserRequest = {
        username,
        displayName: userForm.displayName.trim() || undefined,
        password: userForm.password,
        roleId: userForm.roleId,
        status: userForm.status,
      }
      await createUser(req)
      ElMessage.success(`用户 ${username} 已创建`)
    } else {
      const patch: UpdateUserRequest = {
        displayName: userForm.displayName.trim(),
        roleId: userForm.roleId,
        status: userForm.status,
      }
      if (userForm.password) {
        patch.password = userForm.password
      }
      await updateUser(userForm.id, patch)
      ElMessage.success('用户信息已更新')
    }
    resetUserForm()
    await loadAll()
  } catch (error) {
    ElMessage.error(describeUsersError('保存用户', error))
  } finally {
    savingUser.value = false
  }
}

async function toggleUserStatus(target: AuthUser): Promise<void> {
  if (!canManage.value) {
    return
  }
  if (target.id === currentUserId.value && target.status !== 'disabled') {
    ElMessage.warning('不能停用当前登录账号')
    return
  }
  const next = target.status === 'disabled' ? 'active' : 'disabled'
  try {
    await updateUser(target.id, { status: next })
    ElMessage.success(next === 'active' ? `已启用 ${target.username}` : `已停用 ${target.username}`)
    await loadAll()
  } catch (error) {
    ElMessage.error(describeUsersError('切换用户状态', error))
  }
}

async function handleDeleteUser(target: AuthUser): Promise<void> {
  if (!canManage.value) {
    return
  }
  try {
    await ElMessageBox.confirm(`确定删除用户「${target.username}」？该操作不可恢复。`, '删除用户', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await deleteUser(target.id)
    ElMessage.success('用户已删除')

    await loadAll()
  } catch (error) {
    ElMessage.error(describeUsersError('删除用户', error))
  }
}

/** ---------------- 授权委派 ---------------- */

const grantForm = reactive({
  visible: false,
  mode: 'create' as 'create' | 'edit',
  id: '',
  principalType: 'user' as GrantPrincipalType,scopeType:'namespace',
  principalId: '',
  clusterId: '',
  namespacesText: '',
  permissions: [] as GrantPermission[],
  expiresAt: '',
})
const savingGrant = ref(false)

function resetGrantForm(): void {
  grantForm.visible = false
  grantForm.mode = 'create'
  grantForm.id = ''
  grantForm.principalType = 'user'
  grantForm.principalId = ''
  grantForm.clusterId = ''
  grantForm.namespacesText = ''
  grantForm.scopeType='namespace';grantForm.permissions = ['read','logs']
  grantForm.expiresAt = ''
}

function openCreateGrant(): void {
  grantForm.mode = 'create'
  grantForm.id = ''
  grantForm.principalType = 'user'
  grantForm.principalId = ''
  grantForm.clusterId = ''
  grantForm.namespacesText = ''
  grantForm.scopeType='namespace';grantForm.permissions = ['read','logs']
  grantForm.expiresAt = ''
  grantForm.visible = true
}

function openEditGrant(target: AuthzGrant): void {
  grantForm.mode = 'edit'
  grantForm.id = target.id
  grantForm.principalType = target.principalType
  grantForm.principalId = target.principalId
  grantForm.clusterId = target.clusterId ?? ''
  grantForm.namespacesText = (target.namespaces ?? []).join(', ')
  grantForm.scopeType=target.scopeType??'namespace';grantForm.permissions = (target.actions ?? target.permissions).slice()
  grantForm.expiresAt = target.expiresAt??''
  grantForm.visible = true
}

function splitNamespaces(text: string): string[] {
  return text
    .split(/[,，;；\s]+/)
    .map((part) => part.trim())
    .filter(Boolean)
}

async function submitGrant(): Promise<void> {
  const principalId = grantForm.principalId.trim()
  if (!principalId) {
    ElMessage.warning('请选择用户')
    return
  }
  const namespaces = splitNamespaces(grantForm.namespacesText)
  if (grantForm.scopeType==='namespace' && namespaces.length === 0) {
    ElMessage.warning('至少授权一个 namespace')
    return
  }
  if (grantForm.permissions.length === 0) {
    ElMessage.warning('至少选择一项权限')
    return
  }
  savingGrant.value = true
  try {
    const req:CreateGrantRequest={userId:principalId,clusterId:grantForm.clusterId,scopeType:grantForm.scopeType,namespaces:grantForm.scopeType==='cluster'?[]:namespaces,actions:grantForm.permissions,expiresAt:grantForm.expiresAt||null}
    if(grantForm.mode==='create')await createGrant(req);else await updateGrant(grantForm.id,req)
    ElMessage.success('授权已保存')
    resetGrantForm()
    await loadAll()
  } catch (error) {
    ElMessage.error(describeUsersError('保存授权委派', error))
  } finally {
    savingGrant.value = false
  }
}

async function handleRevokeGrant(target: AuthzGrant): Promise<void> {
  if (!canManage.value) {
    return
  }
  try {
    await ElMessageBox.confirm(
      `确定撤销对「${target.principalId}」的授权委派？立即生效，集群侧 RBAC 回收在 P4.6 落地。`,
      '撤销授权委派',
      {
        confirmButtonText: '撤销',
        cancelButtonText: '取消',
        type: 'warning',
      },
    )
  } catch {
    return
  }
  try {
    await deleteGrant(target.id)
    ElMessage.success('授权委派已撤销')
    await loadAll()
  } catch (error) {
    ElMessage.error(describeUsersError('撤销授权委派', error))
  }
}

</script>

<template>
  <section class="users-page">
    <div class="page-head">
      <h2 class="title">
        <el-icon class="title-icon"><User /></el-icon>
        用户与权限
      </h2>
      <span class="desc">账号、角色权限矩阵、集群资源授权与企业登录。</span>
      <el-button text class="refresh" :icon="Refresh" :loading="loading" @click="loadAll">刷新</el-button>
    </div>

    <TechEmptyState
      v-if="!canManage"
      title="无平台管理权限"
      hint="需要 user.manage 权限才能管理用户与授权；当前角色仅可查看本页入口。"
    />
    <template v-else>
      <div class="stat-grid">
        <div class="card stat">
          <div class="l">用户总数</div>
          <div class="v mono">{{ users.length }}</div>
        </div>
        <div class="card stat">
          <div class="l">已停用</div>
          <div class="v mono mute">{{ users.filter((u) => u.status === 'disabled').length }}</div>
        </div>
        <div class="card stat">
          <div class="l">授权委派</div>
          <div class="v mono">{{ grants.length }}</div>
        </div>

      </div>

      <div class="users-layout">
        <aside class="users-rail" aria-label="用户与权限分区">
          <button
            v-for="meta in SECTION_META"
            :key="meta.key"
            type="button"
            class="rail-item"
            :class="{ active: activeSection === meta.key }"
            :aria-label="meta.title"
            @click="activeSection = meta.key"
          >
            <span class="rail-title">{{ meta.title }}</span>
            <span class="rail-desc">{{ meta.desc }}</span>
          </button>
        </aside>

        <div class="users-content">
          <!-- 用户列表 -->
          <div v-if="activeSection === 'users'" class="card panel">
            <div class="row panel-head">
              <div class="grow">
                <div class="sub strong">账号列表</div>
                <div class="muted small">密码以 argon2id 哈希落库，列表不回显任何口令。</div>
              </div>
              <el-button type="primary" :icon="Plus" @click="openCreateUser">创建用户</el-button>
            </div>
            <div v-if="loading" class="muted center-hint">加载中…</div>
            <div v-else-if="users.length === 0" class="muted center-hint">暂无用户，可创建第一个账号。</div>
            <div v-else class="list">
              <div v-for="target in users" :key="target.id" class="row list-item">
                <div class="grow identity">
                  <div class="row top">
                    <span class="t mono">{{ target.username }}</span>
                    <span class="pill role">{{ roleName(target.roleId) }}</span>
                    <span class="pill" :class="userStatusType(target.status)">{{ userStatusText(target.status) }}</span>
                  </div>
                  <div class="muted meta">
                    <span>{{ target.displayName || '—' }}</span>
                    <span class="faint">·</span>
                    <span class="mono faint">{{ target.id }}</span>
                  </div>
                </div>
                <span class="faint small mono">创建于 {{ formatTime(target.createdAt) }}</span>
                <el-button size="small" text :icon="Edit" @click="openEditUser(target)">编辑</el-button>
                <el-button
                  v-if="target.id !== currentUserId"
                  size="small"
                  text
                  :icon="target.status === 'disabled' ? Unlock : Lock"
                  @click="toggleUserStatus(target)"
                >
                  {{ target.status === 'disabled' ? '启用' : '停用' }}
                </el-button>
                <el-button
                  v-if="target.id !== currentUserId"
                  size="small"
                  text
                  type="danger"
                  :icon="Delete"
                  @click="handleDeleteUser(target)"
                >
                  删除
                </el-button>
              </div>
            </div>
          </div>

          <!-- 角色权限矩阵 -->
          <div v-if="activeSection === 'roles'" class="card panel">
            <div class="row panel-head">
              <div class="grow">
                <div class="sub strong">角色与权限矩阵</div>
                <div class="muted small">角色决定平台管理能力。操作任何集群都需要单独的资源授权。</div>
              </div>
            </div>
            <div v-if="loading" class="muted center-hint">加载中…</div>
            <div v-else-if="roles.length === 0" class="muted center-hint">暂无角色数据。</div>
            <div v-else class="role-list">
              <div v-for="role in roles" :key="role.id" class="role-card">
                <div class="row role-head">
                  <span class="t mono">{{ role.id }}</span>
                  <span class="pill role">{{ role.name }}</span>
                </div>
                <p v-if="role.description" class="muted small role-desc">{{ role.description }}</p>
                <div class="perm-list">
                  <span
                    v-for="permission in role.permissions"
                    :key="permission"
                    class="pill perm"
                    :class="{ wildcard: permission === '*' }"
                  >
                    {{ permission === '*' ? '全量权限 *' : permission }}
                  </span>
                  <span v-if="role.permissions.length === 0" class="faint small">无显式权限</span>
                </div>
              </div>
            </div>
          </div>

          <!-- 授权委派 -->
          <div v-if="activeSection === 'grants'" class="card panel">
            <div class="row panel-head">
              <div class="grow">
                <div class="sub strong">授权委派</div>
                <div class="muted small">按用户/组授予 namespace 权限；destroy 为销毁委派（带有效期）。</div>
              </div>
              <el-button type="primary" :icon="Plus" @click="openCreateGrant">新建委派</el-button>
            </div>
            <div v-if="loading" class="muted center-hint">加载中…</div>
            <div v-else-if="grants.length === 0" class="muted center-hint">暂无授权委派。</div>
            <div v-else class="list">
              <div v-for="grant in grants" :key="grant.id" class="row list-item">
                <div class="grow identity">
                  <div class="row top">
                    <span class="t">{{ principalTypeText(grant.principalType) }}</span>
                    <span class="t mono">{{
                      grant.principalType === 'group' ? `@${grant.principalId}` : grant.principalId
                    }}</span>
                    <span
                      v-for="permission in grant.permissions"
                      :key="permission"
                      class="pill"
                      :class="permission === 'destroy' ? 'danger' : 'ok'"
                    >
                      {{ grantPermissionText(permission) }}
                    </span>
                  </div>
                  <div class="muted meta">
                    <span>namespace</span>
                    <span class="mono">{{ escapeNamespaces(grant.namespaces ?? []) }}</span>
                    <span v-if="grant.clusterId" class="faint">· {{ grant.clusterId }}</span>
                  </div>
                </div>
                <div class="meta expiry">
                  <span v-if="grant.expiresAt" class="faint small mono">至 {{ formatTime(grant.expiresAt) }}</span>
                  <span v-else class="pill mute">长期有效</span>
                </div>
                <el-button size="small" text :icon="Edit" @click="openEditGrant(grant)">编辑</el-button>
                <el-button size="small" text type="danger" :icon="Delete" @click="handleRevokeGrant(grant)">
                  撤销
                </el-button>
              </div>
            </div>
          </div>

          <!-- API Token -->
          <EnterpriseLoginPanel v-if="activeSection === 'enterprise'" />
        </div>
      </div>
    </template>

    <!-- 创建 / 编辑用户 -->
    <el-dialog
      :model-value="userForm.visible"
      :title="userForm.mode === 'create' ? '创建用户' : '编辑用户'"
      width="440px"
      :close-on-click-modal="false"
      @update:model-value="(value: boolean) => !value && resetUserForm()"
      @closed="resetUserForm"
    >
      <el-form label-position="top" @submit.prevent="submitUser">
        <el-form-item label="用户名">
          <el-input v-model="userForm.username" :disabled="userForm.mode === 'edit'" placeholder="登录账号" />
        </el-form-item>
        <el-form-item label="显示名">
          <el-input v-model="userForm.displayName" placeholder="姓名或备注（可选）" />
        </el-form-item>
        <el-form-item :label="userForm.mode === 'create' ? '初始密码' : '重置密码（可选）'">
          <el-input
            v-model="userForm.password"
            type="password"
            show-password
            :placeholder="userForm.mode === 'create' ? '至少 8 位' : '留空则保持原密码'"
          />
        </el-form-item>
        <el-form-item label="角色">
          <el-select v-model="userForm.roleId" class="w-full" placeholder="选择角色">
            <el-option v-for="role in roles" :key="role.id" :label="role.name" :value="role.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="状态">
          <el-switch
            v-model="userForm.status"
            active-value="active"
            inactive-value="disabled"
            active-text="启用"
            inactive-text="停用"
          />
        </el-form-item>
        <p v-if="userForm.mode === 'edit' && userForm.id === currentUserId" class="hint small">
          不能停用或变更当前登录账号的角色（后端同样强校验）。
        </p>
        <div class="row dialog-actions">
          <el-button @click="resetUserForm">取消</el-button>
          <el-button type="primary" :loading="savingUser" :disabled="savingUser" @click="submitUser">
            保存
          </el-button>
        </div>
      </el-form>
    </el-dialog>

    <!-- 创建 / 编辑授权委派 -->
    <el-dialog
      :model-value="grantForm.visible"
      :title="grantForm.mode === 'create' ? '新建授权委派' : '编辑授权委派'"
      width="480px"
      :close-on-click-modal="false"
      @closed="resetGrantForm"
    >
      <el-form label-position="top" @submit.prevent="submitGrant">
        <el-form-item label="用户"><el-select v-model="grantForm.principalId" filterable :disabled="grantForm.mode==='edit'"><el-option v-for="u in users.filter(u=>u.status==='active' && roleName(u.roleId)!=='admin')" :key="u.id" :value="u.id" :label="u.username"/></el-select></el-form-item>
        <el-form-item label="集群"><el-select v-model="grantForm.clusterId" :disabled="grantForm.mode==='edit'"><el-option v-for="c in registered" :key="c.id" :value="c.id" :label="c.name"/></el-select></el-form-item>
        <el-form-item label="资源范围"><el-select v-model="grantForm.scopeType" @change="grantForm.permissions=['read']"><el-option label="命名空间内的资源" value="namespace"/><el-option label="集群级对象（不含命名空间内的资源）" value="cluster"/></el-select></el-form-item>
        <el-form-item v-if="grantForm.scopeType==='namespace'" label="命名空间"><el-input v-model="grantForm.namespacesText" placeholder="多个名称用逗号分隔；* 包含现在和未来全部命名空间" /></el-form-item>
        <el-form-item label="常用授权"><el-button v-for="(label,n) in ['只读诊断','普通变更','允许删除']" :key="label" @click="grantForm.permissions=['read',...(grantForm.scopeType==='namespace'?['logs']:[]),...(['operate','destroy'].slice(0,n))]">{{label}}</el-button></el-form-item>
        <el-form-item label="允许的动作"><el-select v-model="grantForm.permissions" multiple><el-option v-for="a in (grantForm.scopeType==='cluster'?['read','operate','destroy','shell']:['read','logs','operate','destroy','exec','secret.read'])" :key="a" :value="a" :label="a"/></el-select></el-form-item>
        <el-form-item label="有效期至（可选）">
          <el-date-picker
            v-model="grantForm.expiresAt"
            type="datetime"
            value-format="YYYY-MM-DDTHH:mm:ssZ"
            placeholder="不填则长期有效"
            class="w-full"
          />
        </el-form-item>
        <div class="row dialog-actions">
          <el-button @click="resetGrantForm">取消</el-button>
          <el-button type="primary" :loading="savingGrant" :disabled="savingGrant" @click="submitGrant">
            保存
          </el-button>
        </div>
      </el-form>
    </el-dialog>

  </section>
</template>

<style scoped>
.users-page {
  display: flex;
  flex-direction: column;
  gap: 14px;
  flex: 1;
  width: 100%;
  min-width: 0;
  min-height: 0;
}

.page-head {
  display: flex;
  align-items: center;
  gap: 12px;
  background: var(--app-surface);
  border: 1px solid var(--app-border);
  border-radius: var(--app-radius);
  padding: 14px 16px;
}

.page-head .title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  font-size: 16px;
}

.title-icon {
  color: var(--app-accent-2);
}

.page-head .desc {
  font-size: 12.5px;
  color: var(--app-muted);
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.refresh {
  margin-left: auto;
}

.stat-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
  gap: 12px;
}

.card {
  background: var(--app-surface);
  border: 1px solid var(--app-border);
  border-radius: var(--app-radius);
}

.stat {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 14px 16px;
  min-width: 0;
}

.stat .l {
  font-size: 12px;
  color: var(--app-muted);
}

.stat .v {
  font-size: 22px;
  font-weight: 650;
}

.stat .v.mute {
  color: var(--app-muted);
}

.users-layout {
  display: flex;
  gap: 14px;
  flex: 1;
  width: 100%;
  min-width: 0;
  min-height: 0;
  align-items: stretch;
}

.users-rail {
  display: flex;
  flex-direction: column;
  gap: 6px;
  flex: none;
  width: 200px;
  min-width: 0;
  min-height: 0;
  overflow-y: auto;
  padding: 8px;
  background: var(--app-surface);
  border: 1px solid var(--app-border);
  border-radius: var(--app-radius);
}

.rail-item {
  display: flex;
  flex-direction: column;
  gap: 2px;
  align-items: flex-start;
  width: 100%;
  padding: 9px 10px;
  border: 1px solid transparent;
  border-radius: 8px;
  background: transparent;
  color: var(--app-sidebar-text);
  text-align: left;
  cursor: pointer;
  transition:
    background 0.15s ease,
    border-color 0.15s ease;
}

.rail-item:hover {
  background: var(--app-surface-hover);
}

.rail-item.active {
  background: var(--app-surface-2);
  border-color: var(--app-hairline);
}

.rail-title {
  font-size: 13px;
  font-weight: 650;
  line-height: 1.4;
}

.rail-item.active .rail-title {
  color: var(--app-accent);
}

.rail-desc {
  font-size: 11.5px;
  line-height: 1.4;
  color: var(--app-muted);
  overflow: hidden;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
}

.users-content {
  flex: 1;
  width: 100%;
  min-width: 0;
  min-height: 0;
  overflow-y: auto;
  overscroll-behavior: contain;
  scrollbar-gutter: stable;
  display: flex;
  flex-direction: column;
}

.panel {
  flex: none;
  width: 100%;
  padding: 16px;
  min-width: 0;
}

.row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.panel-head {
  width: 100%;
  padding-bottom: 12px;
  margin-bottom: 10px;
  border-bottom: 1px solid var(--app-hairline);
}

.grow {
  min-width: 0;
  flex: 1;
}

.sub {
  font-size: 14px;
}

.strong {
  font-weight: 650;
}

.pill {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  flex: none;
  padding: 2px 8px;
  font-size: 11px;
  line-height: 1.4;
  border-radius: 999px;
  background: rgba(99, 102, 241, 0.12);
  color: var(--app-accent);
}

.pill.ok {
  background: rgba(52, 211, 153, 0.12);
  color: #34d399;
}

.pill.danger {
  background: rgba(248, 113, 113, 0.12);
  color: #f87171;
}

.pill.mute {
  background: var(--app-surface-hover);
  color: var(--app-muted);
}

.list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.list-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px;
  border: 1px solid var(--app-hairline);
  border-radius: 8px;
  background: var(--app-surface-2);
  width: 100%;
  box-sizing: border-box;
}

.identity {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.top {
  flex-wrap: wrap;
}

.t {
  font-size: 14px;
  font-weight: 650;
}

.meta {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  font-size: 11.5px;
}

.expiry {
  flex: none;
}

.mono {
  font-family: var(--app-mono, ui-monospace, SFMono-Regular, Menlo, monospace);
}

.faint {
  color: var(--app-faint, var(--app-muted));
  opacity: 0.8;
}

.muted {
  color: var(--app-muted);
}

.center-hint {
  padding: 24px 8px;
  text-align: center;
  font-size: 12.5px;
  color: var(--app-muted);
}

.small {
  font-size: 12px;
}

.role-list {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 12px;
}

.role-card {
  padding: 14px;
  border: 1px solid var(--app-hairline);
  border-radius: 8px;
  background: var(--app-surface-2);
  min-width: 0;
}

.role-head {
  margin-bottom: 6px;
}

.role-desc {
  margin: 0 0 10px;
  line-height: 1.5;
}

.perm-list {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.pill.role {
  background: rgba(34, 211, 238, 0.12);
  color: #22d3ee;
}

.pill.perm {
  background: var(--app-surface-hover);
  color: var(--app-strong);
  font-family: var(--app-mono, ui-monospace, SFMono-Regular, Menlo, monospace);
}

.pill.perm.wildcard {
  background: rgba(99, 102, 241, 0.14);
  color: var(--app-accent);
}

.user-select {
  width: 180px;
  flex: none;
}

.w-full {
  width: 100%;
}

.hint {
  margin: 0 0 10px;
  color: var(--app-muted);
  line-height: 1.5;
}

.dialog-actions {
  justify-content: flex-end;
  width: 100%;
  margin-top: 4px;
}

.token-box {
  width: 100%;
  box-sizing: border-box;
  padding: 12px;
  margin-bottom: 12px;
  border: 1px solid var(--app-hairline);
  border-radius: 8px;
  background: var(--app-surface-2);
  overflow-x: auto;
  user-select: all;
}

.token-box .mono {
  font-size: 12px;
  color: var(--app-strong);
  word-break: break-all;
}

@media (max-width: 900px) {
  .users-page {
    flex: none;
  }
  .page-head {
    align-items: flex-start;
    flex-wrap: wrap;
  }
  .page-head .title {
    white-space: nowrap;
  }
  .page-head .desc {
    order: 3;
    flex: 1 0 100%;
    white-space: normal;
  }
  .users-layout {
    flex-direction: column;
  }
  .users-rail {
    flex-direction: row;
    flex-wrap: wrap;
    width: 100%;
    overflow: visible;
  }
  .rail-item {
    width: auto;
    min-width: 120px;
  }
  .users-content {
    overflow: visible;
    scrollbar-gutter: auto;
  }
  .list-item {
    flex-wrap: wrap;
  }
}
</style>
