// 用户/角色管理（P3.11a /users /roles）的应用服务。
// 安全约定：密码用 argon2id 哈希落库，PasswordHash 由模型 json:"-" 遮挡，
// 任何返回体/日志/审计都不携带明文或哈希；最后一个本地管理员由 Store 事务保护。
package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

// 用户管理哨兵错误（映射到统一错误信封）。
var (
	ErrUserNotFound  = errors.New("auth: user not found")
	ErrUserExists    = errors.New("auth: user already exists")
	ErrRoleNotFound  = errors.New("auth: role not found")
	ErrInvalidUser   = errors.New("auth: invalid user data")
	ErrProtectedUser = store.ErrProtectedUser
)

// minPasswordLength 是新设/重置密码的最短长度。
const minPasswordLength = 8

// allowedRoleNames 是平台开放的角色白名单：user / operator / admin 三角色。
var allowedRoleNames = map[string]bool{
	RoleUser: true,

	RoleAdmin: true,
}

// CreateUserRequest 是 POST /api/v1/users 的请求体。
type CreateUserRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
	RoleID      string `json:"roleId"`
	Status      string `json:"status"`
}

// UpdateUserRequest 是 PATCH /api/v1/users/{id} 的部分更新体。
// 不支持改用户名：内置 admin 用户名受保护，普通用户改名会引入唯一性/权限表
// 关联复杂度，当前阶段不开放。
type UpdateUserRequest struct {
	DisplayName *string `json:"displayName"`
	Password    *string `json:"password"`
	RoleID      *string `json:"roleId"`
	Status      *string `json:"status"`
}

// ListUsers 返回全部用户（敏感字段不回读）。
func (s *Service) ListUsers(ctx context.Context) ([]*store.User, error) {
	list, err := s.users.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list users: %w", err)
	}
	out := make([]*store.User, 0, len(list))
	for _, u := range list {
		if u.Status != "deleted" {
			out = append(out, u)
		}
	}
	return out, nil
}

// ListRoles 返回全部角色及其权限（管理端只读视图）。
func (s *Service) ListRoles(ctx context.Context) ([]*store.Role, error) {
	// 角色目录是平台固定契约。即使数据库由旧版本初始化或启动修复未执行，
	// 管理端读取时也先幂等补齐三角色，避免创建用户下拉缺项。
	if err := s.reconcileRoles(ctx); err != nil {
		return nil, err
	}
	list, err := s.roles.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list roles: %w", err)
	}
	out := make([]*store.Role, 0, len(allowedRoleNames))
	for _, role := range list {
		if allowedRoleNames[role.Name] {
			out = append(out, role)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return roleOrder(out[i].Name) < roleOrder(out[j].Name)
	})
	return out, nil
}

func roleOrder(name string) int {
	switch name {
	case RoleUser:
		return 0

	case RoleAdmin:
		return 2
	default:
		return 3
	}
}

// CreateUser 校验并创建用户：用户名非空且唯一、密码最短 8 位、
// 角色必须存在且命中三角色白名单、状态仅 active|disabled。
func (s *Service) CreateUser(ctx context.Context, req CreateUserRequest) (*store.User, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return nil, fmt.Errorf("%w: username is required", ErrInvalidUser)
	}
	if req.Password == "" || len(req.Password) < minPasswordLength {
		return nil, fmt.Errorf("%w: password must be at least %d characters", ErrInvalidUser, minPasswordLength)
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "disabled" {
		return nil, fmt.Errorf("%w: status must be active or disabled", ErrInvalidUser)
	}
	role, err := s.validateRole(ctx, req.RoleID)
	if err != nil {
		return nil, err
	}
	existing, err := s.users.GetByUsername(ctx, username)
	if err == nil {
		return nil, fmt.Errorf("%w: %s", ErrUserExists, existing.Username)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("auth: check username: %w", err)
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	u := &store.User{
		ID:           newSecretToken(),
		Username:     username,
		DisplayName:  strings.TrimSpace(req.DisplayName),
		PasswordHash: hash,
		RoleID:       role.ID,
		Status:       status,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, fmt.Errorf("auth: create user: %w", err)
	}
	return u, nil
}

// UpdateUser 支持 displayName / password / roleID / status 的部分更新。
// 最后一个可用本地管理员由 Store 事务保护，安全字段变化撤销旧令牌。
func (s *Service) UpdateUser(ctx context.Context, id string, req UpdateUserRequest) (*store.User, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrUserNotFound
	}
	u, err := s.users.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("auth: get user: %w", err)
	}

	if u.Status == "deleted" {
		return nil, ErrUserNotFound
	}
	versionBefore := u.SessionVersion
	if req.DisplayName != nil {
		u.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status != "active" && status != "disabled" {
			return nil, fmt.Errorf("%w: status must be active or disabled", ErrInvalidUser)
		}
		u.Status = status
	}
	if req.RoleID != nil {
		role, rerr := s.validateRole(ctx, *req.RoleID)
		if rerr != nil {
			return nil, rerr
		}
		u.RoleID = role.ID
	}
	if req.Password != nil {
		if u.PasswordHash == "" {
			return nil, fmt.Errorf("%w: OIDC-only user has no local password", ErrInvalidUser)
		}
		pw := *req.Password
		if pw == "" || len(pw) < minPasswordLength {
			return nil, fmt.Errorf("%w: password must be at least %d characters", ErrInvalidUser, minPasswordLength)
		}
		hash, herr := HashPassword(pw)
		if herr != nil {
			return nil, herr
		}
		u.PasswordHash = hash
	}
	if err := s.users.Update(ctx, u); err != nil {
		return nil, fmt.Errorf("auth: update user: %w", err)
	}
	if u.SessionVersion != versionBefore {
		s.revokeUser(u.ID)
	}
	return u, nil
}

// DeleteUser 保留内部身份引用并注销用户，最后一个本地管理员不能被删除。
func (s *Service) DeleteUser(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return ErrUserNotFound
	}
	u, err := s.users.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrUserNotFound
		}
		return fmt.Errorf("auth: get user: %w", err)
	}

	if u.Status == "deleted" {
		return ErrUserNotFound
	}
	u.Status = "deleted"
	u.Username = "deleted-" + u.ID
	if err := s.users.Update(ctx, u); err != nil {
		return fmt.Errorf("auth: delete user: %w", err)
	}
	s.revokeUser(u.ID)
	return nil
}

// ChangePassword 修改当前用户口令（P6.12 自助改密）：必须携带旧密码，
// 校验通过后用 argon2id 重哈希新密码并落库。新密码沿用平台最小长度
// （8 位）。错误口令统一返回 ErrInvalidCredentials，不区分账号状态。
// 所有已有登录令牌随用户版本变化撤销，需重新登录。
func (s *Service) ChangePassword(ctx context.Context, userID, oldPassword, newPassword string) error {
	if strings.TrimSpace(userID) == "" {
		return ErrInvalidCredentials
	}
	if oldPassword == "" || newPassword == "" {
		return ErrInvalidCredentials
	}
	u, err := s.users.Get(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidCredentials
		}
		return fmt.Errorf("auth: get user: %w", err)
	}
	if u.Status != "active" {
		return ErrInvalidCredentials
	}
	ok, err := verifyPassword(u.PasswordHash, oldPassword)
	if err != nil || !ok {
		return ErrInvalidCredentials
	}
	if newPassword == oldPassword {
		return fmt.Errorf("%w: new password must differ from the old one", ErrInvalidUser)
	}
	if len(newPassword) < minPasswordLength {
		return fmt.Errorf("%w: password must be at least %d characters", ErrInvalidUser, minPasswordLength)
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	u.PasswordHash = hash
	u.UpdatedAt = s.now().UTC()
	if err := s.users.Update(ctx, u); err != nil {
		return fmt.Errorf("auth: update password: %w", err)
	}
	s.revokeUser(u.ID)
	return nil
}

// validateRole 解析并校验角色存在于角色表且名称命中三角色白名单。
func (s *Service) validateRole(ctx context.Context, roleID string) (*store.Role, error) {
	roleID = strings.TrimSpace(roleID)
	if roleID == "" {
		return nil, fmt.Errorf("%w: roleId is required", ErrRoleNotFound)
	}
	role, err := s.roles.Get(ctx, roleID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrRoleNotFound
		}
		return nil, fmt.Errorf("auth: get role: %w", err)
	}
	if !allowedRoleNames[role.Name] {
		return nil, fmt.Errorf("%w: role %q is not allowed", ErrInvalidUser, role.Name)
	}
	return role, nil
}
