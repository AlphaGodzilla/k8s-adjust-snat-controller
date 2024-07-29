package main

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"

	"github.com/volcengine/volcengine-go-sdk/service/natgateway"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
	"github.com/volcengine/volcengine-go-sdk/volcengine/session"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	AK = "AK"
	SK = "SK"
)

func main() {
	// 目标：获取pod上面的注解
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	// labels.NewRequirement("adjust-snat-controller.alphagodzilla/", op selection.Operator, vals []string, opts ...field.PathOption)
	// fields.EscapeValue(s string)
	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.annotations[%s]=[%s]", "adjust-snat-controller.alphagodzilla/enable", "true"),
	})
	if len(pods.Items) <= 0 {
		return
	}
	natGatewayIdMap := make(map[string][]*v1.Pod, 0)
	for _, pod := range pods.Items {
		ann := pod.Annotations
		_, ok := ann["adjust-snat-controller.alphagodzilla/enable"]
		if !ok {
			continue
		}
		ngi, ok := ann["adjust-snat-controller.alphagodzilla/nat-gateway-id"]
		if !ok {
			continue
		}
		_, ok = ann["adjust-snat-controller.alphagodzilla/eip"]
		if !ok {
			continue
		}
		entry, ok := natGatewayIdMap[ngi]
		if !ok {
			natGatewayIdMap[ngi] = make([]*v1.Pod, 0)
			natGatewayIdMap[ngi] = append(natGatewayIdMap[ngi], &pod)
		} else {
			entry = append(entry, &pod)
		}
	}
	for ngi, tpods := range natGatewayIdMap {
		snat_map := list_nat_snat_config("", "", "ap-southeast-1", ngi)
		if len(*snat_map) <= 0 {
			continue
		}
		for _, pod := range tpods {
			podIp := pod.Status.PodIP
			// current eip
			currentEip, ok := (*snat_map)[fmt.Sprintf("%s/32", podIp)]
			if !ok {
				continue
			}
			// expect eip
			ann := pod.Annotations
			expectEip, _ := ann["adjust-snat-controller.alphagodzilla/eip"]
			if *currentEip != expectEip {
				// IP不一致，说明Pod发生漂移
			}
		}
	}
}

func list_nat_snat_config(ak string, sk string, region string, natGatewayId string) *map[string]*string {
	config := volcengine.NewConfig().
		WithRegion(region).
		WithCredentials(credentials.NewStaticCredentials(ak, sk, ""))
	sess, err := session.NewSession(config)
	if err != nil {
		panic(err)
	}
	svc := natgateway.New(sess)

	describeNatGatewaysInput := &natgateway.DescribeNatGatewaysInput{
		NatGatewayIds: volcengine.StringSlice([]string{natGatewayId}),
	}
	resp, err := svc.DescribeNatGateways(describeNatGatewaysInput)
	if err != nil {
		panic(err)
	}
	// fmt.Println(resp)
	snat_map := make(map[string]*string, 0)
	for _, natEntry := range resp.NatGateways {
		snatIds := make([]*string, 0)
		for _, snatEntryId := range natEntry.SnatEntryIds {
			// snatIds[0] =
			snatIds = append(snatIds, snatEntryId)
		}
		if len(snatIds) <= 0 {
			continue
		}
		describeSnatEntriesInput := &natgateway.DescribeSnatEntriesInput{
			NatGatewayId: natEntry.NatGatewayId,
			SnatEntryIds: snatIds,
		}
		snatResp, err := svc.DescribeSnatEntries(describeSnatEntriesInput)
		if err != nil {
			panic(err)
		}
		for _, snatEntry := range snatResp.SnatEntries {
			sourceCidr := snatEntry.SourceCidr
			eipAddress := snatEntry.EipAddress
			//snat_entries = append(snat_entries, map[string]*string{
			//	"sourceCidr": sourceCidr,
			//	"eipAddress": eipAddress,
			//})
			snat_map[*sourceCidr] = eipAddress
		}
	}
	return &snat_map
}
