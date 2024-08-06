package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/volcengine/volcengine-go-sdk/service/natgateway"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
	"github.com/volcengine/volcengine-go-sdk/volcengine/session"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	AK                 = "AK"
	SK                 = "SK"
	REGION             = "REGION"
	LABLE_ENABLE       = "adjust-snat-controller.alphagodzilla/enable"
	ANNO_NATGATEWAY_ID = "adjust-snat-controller.alphagodzilla/nat-gateway-id"
	ANNO_EIP           = "adjust-snat-controller.alphagodzilla/eip-id"
)

func getEnv(key string, defaultVal string) string {
	value := os.Getenv(key)
	if value == "" {
		if defaultVal != "" {
			return defaultVal
		}
		panic(fmt.Sprintf("环境变量 %v 为空", key))
	}
	return value
}

func main() {
	//var config *rest.Config
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()
	config, err := rest.InClusterConfig()
	if err != nil {
		// 使用 KubeConfig 文件创建集群配置
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	}
	// 目标：获取pod上面的注解
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	pods, err := clientSet.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		LabelSelector: LABLE_ENABLE + "=true",
	})
	if err != nil {
		panic(err.Error())
	} else {
		//fmt.Printf("匹配的Pod列表; %v\n", pods)
	}
	fmt.Printf("匹配到pod数量: %v\n", len(pods.Items))
	if len(pods.Items) <= 0 {
		return
	}
	natGatewayIdMap := make(map[string][]*v1.Pod)
	// 对Pod进行排序
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].Name < pods.Items[j].Name
	})
	for _, pod := range pods.Items {
		labels := pod.Labels
		_, ok := labels[LABLE_ENABLE]
		if !ok {
			continue
		}
		ann := pod.Annotations
		ngi, ok := ann[ANNO_NATGATEWAY_ID]
		if !ok {
			continue
		}
		eip, ok := ann[ANNO_EIP]
		if !ok {
			continue
		}
		fmt.Printf("POD: %v, ngi=%v, eip=%v\n", pod.Name, ngi, eip)
		entry, ok := natGatewayIdMap[ngi]
		if !ok {
			natGatewayIdMap[ngi] = make([]*v1.Pod, 0)
			natGatewayIdMap[ngi] = append(natGatewayIdMap[ngi], &pod)
		} else {
			natGatewayIdMap[ngi] = append(entry, &pod)
		}
	}
	//fmt.Printf("NAT-POD列表: %v\n", natGatewayIdMap)
	// 创建VE的客户端
	veClient := createClient(getEnv(AK, ""), getEnv(SK, ""), getEnv(REGION, "ap-southeast-1"))
	for ngi, tpods := range natGatewayIdMap {
		snatMap := listNatSnatConfig(veClient, ngi)
		if len(*snatMap) <= 0 {
			continue
		}
		for index, pod := range tpods {
			// expect pod bind eip
			ann := pod.Annotations
			eipIds, _ := ann[ANNO_EIP]
			eipParts := strings.Split(eipIds, ",")
			eipPartMaxIndex := len(eipParts) - 1
			if index > eipPartMaxIndex {
				continue
			}
			eipId := eipParts[index]
			fmt.Printf("生成执行计划, pod=%v, eip=%v\n", pod.Name, eipId)
			if eipId == "" {
				continue
			}
			snatItem, ok := (*snatMap)[eipId]
			podIp := pod.Status.PodIP
			if !ok {
				// 没有相对应的SNAT规则，就创建新的规则
				snatName := pod.Name
				fmt.Printf("计划: 创建SNAT规则, snatName=%v, sourceCidr=%v/32\n", snatName, podIp)
				createSnat(veClient, ngi, eipId, snatName, fmt.Sprintf("%v/32", podIp))
				continue
			}
			sourceCidr := snatItem["sourceCidr"]
			if !strings.HasPrefix(sourceCidr, podIp) {
				// 不一致，说明Pod发生漂移. 修改SNAT规则
				log.Printf("Pod发生漂移IP变化，预期IP=%v， 当前IP=%v， 动作：修改SNAT规则\n", sourceCidr, podIp)
				// 删除旧的SNAT规则
				snatId := snatItem["snatId"]
				fmt.Printf("计划: 删除SNAT规则,snatId=%v, 然后创建新SNAT规则\n", snatId)
				deleteSnat(veClient, snatId)
				// 创建新的SNAT规则
				snatName := pod.Name
				fmt.Printf("计划: 创建SNAT规则, snatName=%v, sourceCidr=%v/32\n", snatName, podIp)
				createSnat(veClient, ngi, eipId, snatName, fmt.Sprintf("%v/32", podIp))
				continue
			}
			fmt.Printf("已全部最新，无需修改\n")
		}
	}
}

func createClient(ak string, sk string, region string) *natgateway.NATGATEWAY {
	config := volcengine.NewConfig().
		WithRegion(region).
		WithCredentials(credentials.NewStaticCredentials(ak, sk, ""))
	sess, err := session.NewSession(config)
	if err != nil {
		panic(err)
	}
	return natgateway.New(sess)
}

func listNatSnatConfig(client *natgateway.NATGATEWAY, natGatewayId string) *map[string]map[string]string {
	describeNatGatewaysInput := &natgateway.DescribeNatGatewaysInput{
		NatGatewayIds: volcengine.StringSlice([]string{natGatewayId}),
	}
	resp, err := client.DescribeNatGateways(describeNatGatewaysInput)
	if err != nil {
		panic(err)
	}
	// fmt.Println(resp)
	snatMap := make(map[string]map[string]string)
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
		snatResp, err := client.DescribeSnatEntries(describeSnatEntriesInput)
		if err != nil {
			panic(err)
		}
		for _, snatEntry := range snatResp.SnatEntries {
			sourceCidr := snatEntry.SourceCidr
			eipId := snatEntry.EipId
			snatEntry := snatEntry.SnatEntryId
			item := make(map[string]string)
			item["snatId"] = *snatEntry
			item["sourceCidr"] = *sourceCidr
			snatMap[*eipId] = item
		}
	}
	return &snatMap
}

func deleteSnat(client *natgateway.NATGATEWAY, snatId string) {
	deleteSnatEntryInput := &natgateway.DeleteSnatEntryInput{
		SnatEntryId: volcengine.String(snatId),
	}

	resp, err := client.DeleteSnatEntry(deleteSnatEntryInput)
	if err != nil {
		panic(err)
	}
	log.Printf("删除SNAT规则，%v, %v\n", snatId, toJsonStr(resp))
}

func createSnat(client *natgateway.NATGATEWAY, NatGatewayId string, EipId string, SnatEntryName string, SourceCidr string) {
	createSnatEntryInput := &natgateway.CreateSnatEntryInput{
		EipId:         volcengine.String(EipId),
		NatGatewayId:  volcengine.String(NatGatewayId),
		SnatEntryName: volcengine.String(SnatEntryName),
		SourceCidr:    volcengine.String(SourceCidr),
	}
	resp, err := client.CreateSnatEntry(createSnatEntryInput)
	if err != nil {
		panic(err)
	}
	log.Printf("创建SNAT规则，%v\n", toJsonStr(resp))
}

func toJsonStr(value any) string {
	json, err := json.Marshal(value)
	if err != nil {
		fmt.Println("Error serializing to JSON:", err)
	}
	return string(json)
}
