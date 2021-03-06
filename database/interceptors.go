package database

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/cel-go/cel"
	"github.com/graphikDB/graphik/gen/grpc/go"
	"github.com/graphikDB/graphik/helpers"
	"github.com/graphikDB/graphik/logger"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	"github.com/lestrrat-go/jwx/jwa"
	"github.com/lestrrat-go/jwx/jws"
	"github.com/pkg/errors"
	"go.etcd.io/bbolt"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"io/ioutil"
	"net/http"
	"time"
)

func (g *Graph) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		token, err := grpc_auth.AuthFromMD(ctx, "Bearer")
		if err != nil {
			return nil, err
		}
		tokenHash := helpers.Hash([]byte(token))
		if val, ok := g.jwtCache.Get(tokenHash); ok {
			payload := val.(map[string]interface{})
			ctx, err := g.check(ctx, info.FullMethod, req, payload)
			if err != nil {
				return nil, err
			}
			return handler(ctx, req)
		}
		ctx = g.methodToContext(ctx, info.FullMethod)
		userinfoReq, err := http.NewRequest(http.MethodGet, g.openID.UserinfoEndpoint, nil)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "failed to get userinfo: %s", err.Error())
		}
		userinfoReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		resp, err := http.DefaultClient.Do(userinfoReq)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "failed to get userinfo: %s", err.Error())
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, status.Errorf(codes.Unauthenticated, "failed to get userinfo: %v", resp.StatusCode)
		}
		bits, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "failed to get userinfo: %s", err.Error())
		}
		payload := map[string]interface{}{}
		if err := json.Unmarshal(bits, &payload); err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "failed to get userinfo: %s", err.Error())
		}
		g.jwtCache.Set(tokenHash, payload, 1*time.Hour)
		ctx, err = g.check(ctx, info.FullMethod, req, payload)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func (g *Graph) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		token, err := grpc_auth.AuthFromMD(ss.Context(), "Bearer")
		if err != nil {
			return err
		}
		tokenHash := helpers.Hash([]byte(token))
		if val, ok := g.jwtCache.Get(tokenHash); ok {
			payload := val.(map[string]interface{})
			ctx, err := g.check(ss.Context(), info.FullMethod, srv, payload)
			if err != nil {
				return err
			}
			wrapped := grpc_middleware.WrapServerStream(ss)
			wrapped.WrappedContext = ctx
			return handler(srv, wrapped)
		}
		ctx := g.methodToContext(ss.Context(), info.FullMethod)
		userinfoReq, err := http.NewRequest(http.MethodGet, g.openID.UserinfoEndpoint, nil)
		if err != nil {
			return status.Errorf(codes.Unauthenticated, "failed to get userinfo: %s", err.Error())
		}
		userinfoReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		resp, err := http.DefaultClient.Do(userinfoReq)
		if err != nil {
			return status.Errorf(codes.Unauthenticated, "failed to get userinfo: %s", err.Error())
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return status.Errorf(codes.Unauthenticated, "failed to get userinfo: %v", resp.StatusCode)
		}
		bits, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return errors.Wrap(err, "failed to get userinfo")
		}
		payload := map[string]interface{}{}
		if err := json.Unmarshal(bits, &payload); err != nil {
			return status.Errorf(codes.Unauthenticated, "failed to get userinfo: %s", err.Error())
		}
		g.jwtCache.Set(tokenHash, payload, 1*time.Hour)
		ctx, err = g.check(ctx, info.FullMethod, srv, payload)
		if err != nil {
			return err
		}
		wrapped := grpc_middleware.WrapServerStream(ss)
		wrapped.WrappedContext = ctx
		return handler(srv, wrapped)
	}
}

func (a *Graph) userToContext(ctx context.Context, payload map[string]interface{}) (context.Context, *apipb.Doc, error) {
	email, ok := payload["email"].(string)
	if !ok {
		return nil, nil, errors.New("email not present in userinfo")
	}
	var (
		doc *apipb.Doc
		err error
	)
	a.db.View(func(tx *bbolt.Tx) error {
		doc, err = a.getDoc(ctx, tx, &apipb.Ref{
			Gtype: string(userType),
			Gid:   email,
		})
		if err != nil {
			return err
		}
		return nil
	})
	if doc == nil {
		logger.Info("creating user", zap.String("email", email))
		strct, err := structpb.NewStruct(payload)
		if err != nil {
			return nil, nil, err
		}
		doc, err = a.createIdentity(ctx, &apipb.DocConstructor{
			Ref: &apipb.RefConstructor{
				Gtype: string(userType),
				Gid:   email,
			},
			Attributes: strct,
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return context.WithValue(ctx, authCtxKey, doc), doc, nil
}

func (s *Graph) getIdentity(ctx context.Context) *apipb.Doc {
	val, ok := ctx.Value(authCtxKey).(*apipb.Doc)
	if ok {
		return val
	}
	val2, ok := ctx.Value(authCtxKey).(apipb.Doc)
	if ok {
		return &val2
	}
	return nil
}

func (r *Graph) getMethod(ctx context.Context) string {
	val, ok := ctx.Value(methodCtxKey).(string)
	if ok {
		return val
	}
	return ""
}

func (r *Graph) methodToContext(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, methodCtxKey, path)
}

func (g *Graph) verifyJWT(token string) (map[string]interface{}, error) {
	message, err := jws.ParseString(token)
	if err != nil {
		return nil, err
	}
	g.jwksMu.RLock()
	defer g.jwksMu.RUnlock()
	if g.jwksSet == nil {
		data := map[string]interface{}{}
		if err := json.Unmarshal(message.Payload(), &data); err != nil {
			return nil, err
		}
		return data, nil
	}
	if len(message.Signatures()) == 0 {
		return nil, fmt.Errorf("zero jws signatures")
	}
	kid, ok := message.Signatures()[0].ProtectedHeaders().Get("kid")
	if !ok {
		return nil, fmt.Errorf("jws kid not found")
	}
	algI, ok := message.Signatures()[0].ProtectedHeaders().Get("alg")
	if !ok {
		return nil, fmt.Errorf("jw alg not found")
	}
	alg, ok := algI.(jwa.SignatureAlgorithm)
	if !ok {
		return nil, fmt.Errorf("alg type cast error")
	}
	keys := g.jwksSet.LookupKeyID(kid.(string))
	if len(keys) == 0 {
		return nil, errors.Errorf("failed to lookup kid: %s - zero keys", kid.(string))
	}
	var key interface{}
	if err := keys[0].Raw(&key); err != nil {
		return nil, err
	}
	payload, err := jws.Verify([]byte(token), alg, key)
	if err != nil {
		return nil, err
	}
	data := map[string]interface{}{}
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func (g *Graph) check(ctx context.Context, method string, req interface{}, payload map[string]interface{}) (context.Context, error) {
	ctx = g.methodToContext(ctx, method)
	ctx, user, err := g.userToContext(ctx, payload)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, err.Error())
	}
	if !g.isGraphikAdmin(user) {
		now := time.Now()
		request := &apipb.Request{
			Method:    method,
			User:      user,
			Timestamp: timestamppb.New(now),
		}
		if val, ok := req.(proto.Message); ok {
			bits, _ := helpers.MarshalJSON(val)
			reqMap := map[string]interface{}{}
			if err := json.Unmarshal(bits, &reqMap); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			request.Request = apipb.NewStruct(reqMap)
		}
		var programs []cel.Program
		g.rangeAuthorizers(func(a *authorizer) bool {
			programs = append(programs, a.program)
			return true
		})
		result, err := g.vm.Auth().Eval(request, programs...)
		if err != nil {
			return nil, err
		}
		if !result {
			return nil, status.Errorf(codes.PermissionDenied, "request from %s.%s  authorization = denied", user.GetRef().GetGtype(), user.GetRef().GetGid())
		}
	}
	return ctx, nil
}

func (g *Graph) isGraphikAdmin(user *apipb.Doc) bool {
	if user.GetAttributes().GetFields() == nil {
		return false
	}
	return helpers.ContainsString(user.GetAttributes().GetFields()["email"].GetStringValue(), g.rootUsers)
}
