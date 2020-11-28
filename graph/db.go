package graph

import (
	"context"
	apipb "github.com/autom8ter/graphik/api"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"go.etcd.io/bbolt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (g *Graph) metaDefaults(ctx context.Context, meta *apipb.Metadata) {
	identity := g.getIdentity(ctx)
	if meta == nil {
		meta = &apipb.Metadata{}
	}
	if meta.GetUpdatedBy() == nil {
		identity.GetMetadata().UpdatedBy = identity.GetPath()
	}
	if meta.GetCreatedAt() == nil {
		meta.CreatedAt = timestamppb.Now()
	}
	if meta.GetUpdatedAt() == nil {
		meta.UpdatedAt = timestamppb.Now()
	}
	meta.Version += 1
}

func (g *Graph) setNode(ctx context.Context, tx *bbolt.Tx, node *apipb.Node) (*apipb.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if node.GetPath() == nil {
		node.Path = &apipb.Path{}
	}
	if node.GetPath().Gid == "" {
		node.Path.Gid = uuid.New().String()
	}
	g.metaDefaults(ctx, node.Metadata)
	bits, err := proto.Marshal(node)
	if err != nil {
		return nil, err
	}
	nodeBucket := tx.Bucket(dbNodes)
	bucket := nodeBucket.Bucket([]byte(node.GetPath().GetGtype()))
	if bucket == nil {
		bucket, err = nodeBucket.CreateBucketIfNotExists([]byte(node.GetPath().GetGtype()))
		if err != nil {
			return nil, err
		}
	}
	if err := bucket.Put([]byte(node.GetPath().GetGid()), bits); err != nil {
		return nil, err
	}
	return node, nil
}

func (g *Graph) setNodes(ctx context.Context, nodes ...*apipb.Node) (*apipb.Nodes, error) {
	var nds = &apipb.Nodes{}
	if err := g.db.Update(func(tx *bbolt.Tx) error {
		for _, node := range nodes {
			n, err := g.setNode(ctx, tx, node)
			if err != nil {
				return err
			}
			nds.Nodes = append(nds.Nodes, n)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return nds, nil
}

func (g *Graph) setEdge(ctx context.Context, tx *bbolt.Tx, edge *apipb.Edge) (*apipb.Edge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nodeBucket := tx.Bucket(dbNodes)
	{
		fromBucket := nodeBucket.Bucket([]byte(edge.From.Gtype))
		if fromBucket == nil {
			return nil, errors.Errorf("from node %s does not exist", edge.From.String())
		}
		val := fromBucket.Get([]byte(edge.From.Gid))
		if val == nil {
			return nil, errors.Errorf("from node %s does not exist", edge.From.String())
		}
	}
	{
		toBucket := nodeBucket.Bucket([]byte(edge.To.Gtype))
		if toBucket == nil {
			return nil, errors.Errorf("to node %s does not exist", edge.To.String())
		}
		val := toBucket.Get([]byte(edge.To.Gid))
		if val == nil {
			return nil, errors.Errorf("to node %s does not exist", edge.To.String())
		}
	}
	if edge.GetPath() == nil {
		edge.Path = &apipb.Path{}
	}
	if edge.GetPath().Gid == "" {
		edge.Path.Gid = uuid.New().String()
	}
	g.metaDefaults(ctx, edge.Metadata)
	bits, err := proto.Marshal(edge)
	if err != nil {
		return nil, err
	}
	edgeBucket := tx.Bucket(dbEdges)
	edgeBucket = edgeBucket.Bucket([]byte(edge.GetPath().GetGtype()))
	if edgeBucket == nil {
		edgeBucket, err = edgeBucket.CreateBucketIfNotExists([]byte(edge.GetPath().GetGtype()))
		if err != nil {
			return nil, err
		}
	}
	if err := edgeBucket.Put([]byte(edge.GetPath().GetGid()), bits); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.edgesFrom[edge.GetFrom().String()] = append(g.edgesFrom[edge.GetFrom().String()], edge.GetPath())
	g.edgesTo[edge.GetTo().String()] = append(g.edgesTo[edge.GetTo().String()], edge.GetPath())
	if !edge.Directed {
		g.edgesTo[edge.GetFrom().String()] = append(g.edgesTo[edge.GetFrom().String()], edge.GetPath())
		g.edgesFrom[edge.GetTo().String()] = append(g.edgesFrom[edge.GetTo().String()], edge.GetPath())
	}
	sortPaths(g.edgesFrom[edge.GetFrom().String()])
	sortPaths(g.edgesTo[edge.GetTo().String()])
	return edge, nil
}

func (g *Graph) setEdges(ctx context.Context, edges ...*apipb.Edge) (*apipb.Edges, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var edgs = &apipb.Edges{}
	if err := g.db.Update(func(tx *bbolt.Tx) error {
		for _, edge := range edges {
			e, err := g.setEdge(ctx, tx, edge)
			if err != nil {
				return err
			}
			edgs.Edges = append(edgs.Edges, e)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	edgs.Sort()
	return edgs, nil
}

func (g *Graph) getNode(ctx context.Context, tx *bbolt.Tx, path *apipb.Path) (*apipb.Node, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	identity := g.getIdentity(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "failed to get identity")
	}
	var node apipb.Node
	bucket := tx.Bucket(dbNodes).Bucket([]byte(path.Gtype))
	if bucket == nil {
		return nil, ErrNotFound
	}
	bits := bucket.Get([]byte(path.Gid))
	if err := proto.Unmarshal(bits, &node); err != nil {
		return nil, err
	}
	return &node, nil
}

func (g *Graph) getEdge(ctx context.Context, tx *bbolt.Tx, path *apipb.Path) (*apipb.Edge, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var edge apipb.Edge
	bucket := tx.Bucket(dbEdges).Bucket([]byte(path.Gtype))
	if bucket == nil {
		return nil, ErrNotFound
	}
	bits := bucket.Get([]byte(path.Gid))
	if err := proto.Unmarshal(bits, &edge); err != nil {
		return nil, err
	}
	return &edge, nil
}

func (g *Graph) rangeEdges(ctx context.Context, gType string, fn func(n *apipb.Edge) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.db.View(func(tx *bbolt.Tx) error {
		if gType == apipb.Any {
			types, err := g.EdgeTypes(ctx)
			if err != nil {
				return err
			}
			for _, edgeType := range types {
				if err := g.rangeEdges(ctx, edgeType, fn); err != nil {
					return err
				}
			}
			return nil
		}
		bucket := tx.Bucket(dbEdges).Bucket([]byte(gType))
		if bucket == nil {
			return ErrNotFound
		}

		return bucket.ForEach(func(k, v []byte) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var edge apipb.Edge
			if err := proto.Unmarshal(v, &edge); err != nil {
				return err
			}
			if !fn(&edge) {
				return DONE
			}
			return nil
		})
	}); err != nil && err != DONE {
		return err
	}
	return nil
}